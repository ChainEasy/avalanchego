// Copyright (C) 2019-2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package merkledb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"

	"github.com/ava-labs/avalanchego/utils"

	"go.opentelemetry.io/otel/attribute"

	oteltrace "go.opentelemetry.io/otel/trace"

	"golang.org/x/exp/slices"
	"golang.org/x/sync/errgroup"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/maybe"
)

const (
	initKeyValuesSize        = 256
	defaultPreallocationSize = 100
)

var (
	_ TrieView = (*trieView)(nil)

	ErrCommitted          = errors.New("view has been committed")
	ErrInvalid            = errors.New("the trie this view was based on has changed, rendering this view invalid")
	ErrOddLengthWithValue = errors.New(
		"the underlying db only supports whole number of byte keys, so cannot record changes with odd nibble length",
	)
	ErrGetPathToFailure       = errors.New("GetPathTo failed to return the closest node")
	ErrStartAfterEnd          = errors.New("start key > end key")
	ErrViewIsNotAChild        = errors.New("passed in view is required to be a child of the current view")
	ErrNoValidRoot            = errors.New("a valid root was not provided to the trieView constructor")
	ErrParentNotDatabase      = errors.New("parent trie is not database")
	ErrNodesAlreadyCalculated = errors.New("cannot modify the trie after the node changes have been calculated")

	numCPU = runtime.NumCPU()
)

type trieView struct {
	// If true, this view has been committed.
	// [commitLock] must be held while accessing this field.
	committed  bool
	commitLock sync.RWMutex

	// tracking bool to enforce that no changes are made to the trie after the nodes have been calculated
	nodesAlreadyCalculated utils.Atomic[bool]

	// calculateNodesOnce is a once to ensure that node calculation only occurs a single time
	calculateNodesOnce sync.Once

	// Controls the trie's validity related fields.
	// Must be held while reading/writing [childViews], [invalidated], and [parentTrie].
	// Only use to lock current trieView or descendants of the current trieView
	// DO NOT grab the [validityTrackingLock] of any ancestor trie while this is held.
	validityTrackingLock sync.RWMutex

	// If true, this view has been invalidated and can't be used.
	//
	// Invariant: This view is marked as invalid before any of its ancestors change.
	// Since we ensure that all subviews are marked invalid before making an invalidating change
	// then if we are still valid at the end of the function, then no corrupting changes could have
	// occurred during execution.
	// Namely, if we have a method with:
	//
	// *Code Accessing Ancestor State*
	//
	// if t.isInvalid() {
	//     return ErrInvalid
	//  }
	// return [result]
	//
	// If the invalidated check passes, then we're guaranteed that no ancestor changes occurred
	// during the code that accessed ancestor state and the result of that work is still valid
	//
	// [validityTrackingLock] must be held when reading/writing this field.
	invalidated bool

	// the uncommitted parent trie of this view
	// [validityTrackingLock] must be held when reading/writing this field.
	parentTrie TrieView

	// The valid children of this trie.
	// [validityTrackingLock] must be held when reading/writing this field.
	childViews []*trieView

	// Changes made to this view.
	// May include nodes that haven't been updated
	// but will when their ID is recalculated.
	changes *changeSummary

	db *merkleDB

	// The root of the trie represented by this view.
	root *node
}

// NewView returns a new view on top of this one.
// Adds the new view to [t.childViews].
// Assumes [t.commitLock] isn't held.
func (t *trieView) NewView(ctx context.Context, batchOps []database.BatchOp) (TrieView, error) {
	if t.isInvalid() {
		return nil, ErrInvalid
	}
	t.commitLock.RLock()
	defer t.commitLock.RUnlock()

	if t.committed {
		return t.getParentTrie().NewView(ctx, batchOps)
	}

	if err := t.calculateNodeIDs(ctx); err != nil {
		return nil, err
	}

	newView, err := newTrieView(t.db, t, t.root.clone(), batchOps)
	if err != nil {
		return nil, err
	}

	t.validityTrackingLock.Lock()
	defer t.validityTrackingLock.Unlock()

	if t.invalidated {
		return nil, ErrInvalid
	}
	t.childViews = append(t.childViews, newView)

	return newView, nil
}

// Creates a new view with the given [parentTrie].
func newTrieView(
	db *merkleDB,
	parentTrie TrieView,
	root *node,
	batchOps []database.BatchOp,
) (*trieView, error) {
	if root == nil {
		return nil, ErrNoValidRoot
	}

	newView := &trieView{
		root:       root,
		db:         db,
		parentTrie: parentTrie,
		changes:    newChangeSummary(len(batchOps)),
	}

	for _, op := range batchOps {
		newVal := maybe.Nothing[[]byte]()
		if !op.Delete {
			newVal = maybe.Some(slices.Clone(op.Value))
		}
		if err := newView.recordValueChange(newPath(op.Key), newVal); err != nil {
			return nil, err
		}
	}
	return newView, nil
}

// Creates a view of the db at a historical root using the provided changes
func newHistoricalTrieView(
	db *merkleDB,
	changes *changeSummary,
) (*trieView, error) {
	if changes == nil {
		return nil, ErrNoValidRoot
	}

	passedRootChange, ok := changes.nodes[RootPath]
	if !ok {
		return nil, ErrNoValidRoot
	}

	newView := &trieView{
		root:       passedRootChange.after,
		db:         db,
		parentTrie: db,
		changes:    changes,
	}
	// since this is a set of historical changes, all nodes have already been calculated
	// since no new changes have occurred, no new calculations need to be done
	newView.calculateNodesOnce.Do(func() {})
	newView.nodesAlreadyCalculated.Set(true)
	return newView, nil
}

// Recalculates the node IDs for all changed nodes in the trie.
func (t *trieView) calculateNodeIDs(ctx context.Context) error {
	var err error
	t.calculateNodesOnce.Do(func() {
		if t.isInvalid() {
			err = ErrInvalid
			return
		}
		defer t.nodesAlreadyCalculated.Set(true)

		// We wait to create the span until after checking that we need to actually
		// calculateNodeIDs to make traces more useful (otherwise there may be a span
		// per key modified even though IDs are not re-calculated).
		ctx, span := t.db.tracer.Start(ctx, "MerkleDB.trieview.calculateNodeIDs")
		defer span.End()

		// add all the changed key/values to the nodes of the trie
		for key, change := range t.changes.values {
			if change.after.IsNothing() {
				if err = t.remove(key); err != nil {
					return
				}
			} else {
				if _, err = t.insert(key, change.after); err != nil {
					return
				}
			}
		}

		// [eg] limits the number of goroutines we start.
		var eg errgroup.Group
		eg.SetLimit(numCPU)
		if err = t.calculateNodeIDsHelper(ctx, t.root, &eg); err != nil {
			return
		}
		if err = eg.Wait(); err != nil {
			return
		}
		t.changes.rootID = t.root.id

		// ensure no ancestor changes occurred during execution
		if t.isInvalid() {
			err = ErrInvalid
			return
		}
	})
	return err
}

// Calculates the ID of all descendants of [n] which need to be recalculated,
// and then calculates the ID of [n] itself.
func (t *trieView) calculateNodeIDsHelper(ctx context.Context, n *node, eg *errgroup.Group) error {
	var (
		// We use [wg] to wait until all descendants of [n] have been updated.
		// Note we can't wait on [eg] because [eg] may have started goroutines
		// that aren't calculating IDs for descendants of [n].
		wg              sync.WaitGroup
		updatedChildren = make(chan *node, len(n.children))
	)

	for childIndex, child := range n.children {
		childIndex, child := childIndex, child

		childPath := n.key + path(childIndex) + child.compressedPath
		childNodeChange, ok := t.changes.nodes[childPath]
		if !ok {
			// This child wasn't changed.
			continue
		}

		wg.Add(1)
		updateChild := func() error {
			defer wg.Done()

			if err := t.calculateNodeIDsHelper(ctx, childNodeChange.after, eg); err != nil {
				return err
			}

			// Note that this will never block
			updatedChildren <- childNodeChange.after
			return nil
		}

		// Try updating the child and its descendants in a goroutine.
		if ok := eg.TryGo(updateChild); !ok {
			// We're at the goroutine limit; do the work in this goroutine.
			if err := updateChild(); err != nil {
				return err
			}
		}
	}

	// Wait until all descendants of [n] have been updated.
	wg.Wait()
	close(updatedChildren)

	for child := range updatedChildren {
		n.addChild(child)
	}

	// The IDs [n]'s descendants are up to date so we can calculate [n]'s ID.
	return n.calculateID(t.db.metrics)
}

// GetProof returns a proof that [bytesPath] is in or not in trie [t].
func (t *trieView) GetProof(ctx context.Context, key []byte) (*Proof, error) {
	_, span := t.db.tracer.Start(ctx, "MerkleDB.trieview.GetProof")
	defer span.End()

	if err := t.calculateNodeIDs(ctx); err != nil {
		return nil, err
	}

	return t.getProof(ctx, key)
}

// Returns a proof that [bytesPath] is in or not in trie [t].
func (t *trieView) getProof(ctx context.Context, key []byte) (*Proof, error) {
	_, span := t.db.tracer.Start(ctx, "MerkleDB.trieview.getProof")
	defer span.End()

	proof := &Proof{
		Key: key,
	}

	// Get the node at the given path, or the node closest to it.
	keyPath := newPath(key)

	proofPath, err := t.getPathTo(keyPath)
	if err != nil {
		return nil, err
	}

	// From root --> node from left --> right.
	proof.Path = make([]ProofNode, len(proofPath), len(proofPath)+1)
	for i, node := range proofPath {
		proof.Path[i] = node.asProofNode()
	}

	closestNode := proofPath[len(proofPath)-1]

	if closestNode.key.Compare(keyPath) == 0 {
		// There is a node with the given [key].
		proof.Value = maybe.Bind(closestNode.value, slices.Clone[[]byte])
		return proof, nil
	}

	// There is no node with the given [key].
	// If there is a child at the index where the node would be
	// if it existed, include that child in the proof.
	nextIndex := keyPath[len(closestNode.key)]
	child, ok := closestNode.children[nextIndex]
	if !ok {
		return proof, nil
	}

	childPath := closestNode.key + path(nextIndex) + child.compressedPath
	childNode, err := t.getNodeFromParent(closestNode, childPath)
	if err != nil {
		return nil, err
	}
	proof.Path = append(proof.Path, childNode.asProofNode())
	if t.isInvalid() {
		return nil, ErrInvalid
	}
	return proof, nil
}

// GetRangeProof returns a range proof for (at least part of) the key range [start, end].
// The returned proof's [KeyValues] has at most [maxLength] values.
// [maxLength] must be > 0.
func (t *trieView) GetRangeProof(
	ctx context.Context,
	start maybe.Maybe[[]byte],
	end maybe.Maybe[[]byte],
	maxLength int,
) (*RangeProof, error) {
	ctx, span := t.db.tracer.Start(ctx, "MerkleDB.trieview.GetRangeProof")
	defer span.End()

	if start.HasValue() && end.HasValue() && bytes.Compare(start.Value(), end.Value()) == 1 {
		return nil, ErrStartAfterEnd
	}

	if maxLength <= 0 {
		return nil, fmt.Errorf("%w but was %d", ErrInvalidMaxLength, maxLength)
	}

	if err := t.calculateNodeIDs(ctx); err != nil {
		return nil, err
	}

	var result RangeProof

	result.KeyValues = make([]KeyValue, 0, initKeyValuesSize)
	it := t.NewIteratorWithStart(start.Value())
	for it.Next() && len(result.KeyValues) < maxLength && (end.IsNothing() || bytes.Compare(it.Key(), end.Value()) <= 0) {
		// clone the value to prevent editing of the values stored within the trie
		result.KeyValues = append(result.KeyValues, KeyValue{
			Key:   it.Key(),
			Value: slices.Clone(it.Value()),
		})
	}
	it.Release()
	if err := it.Error(); err != nil {
		return nil, err
	}

	// This proof may not contain all key-value pairs in [start, end] due to size limitations.
	// The end proof we provide should be for the last key-value pair in the proof, not for
	// the last key-value pair requested, which may not be in this proof.
	var (
		endProof *Proof
		err      error
	)
	if len(result.KeyValues) > 0 {
		greatestKey := result.KeyValues[len(result.KeyValues)-1].Key
		endProof, err = t.getProof(ctx, greatestKey)
		if err != nil {
			return nil, err
		}
	} else if end.HasValue() {
		endProof, err = t.getProof(ctx, end.Value())
		if err != nil {
			return nil, err
		}
	}
	if endProof != nil {
		result.EndProof = endProof.Path
	}

	if start.HasValue() {
		startProof, err := t.getProof(ctx, start.Value())
		if err != nil {
			return nil, err
		}
		result.StartProof = startProof.Path

		// strip out any common nodes to reduce proof size
		i := 0
		for ; i < len(result.StartProof) &&
			i < len(result.EndProof) &&
			result.StartProof[i].KeyPath.Equal(result.EndProof[i].KeyPath); i++ {
		}
		result.StartProof = result.StartProof[i:]
	}

	if len(result.StartProof) == 0 && len(result.EndProof) == 0 && len(result.KeyValues) == 0 {
		// If the range is empty, return the root proof.
		rootProof, err := t.getProof(ctx, rootKey)
		if err != nil {
			return nil, err
		}
		result.EndProof = rootProof.Path
	}

	if t.isInvalid() {
		return nil, ErrInvalid
	}
	return &result, nil
}

// CommitToDB commits changes from this trie to the underlying DB.
func (t *trieView) CommitToDB(ctx context.Context) error {
	ctx, span := t.db.tracer.Start(ctx, "MerkleDB.trieview.CommitToDB")
	defer span.End()

	t.db.commitLock.Lock()
	defer t.db.commitLock.Unlock()

	return t.commitToDB(ctx)
}

// Commits the changes from [trieToCommit] to this view,
// this view to its parent, and so on until committing to the db.
// Assumes [t.db.commitLock] is held.
func (t *trieView) commitToDB(ctx context.Context) error {
	t.commitLock.Lock()
	defer t.commitLock.Unlock()

	ctx, span := t.db.tracer.Start(ctx, "MerkleDB.trieview.commitToDB", oteltrace.WithAttributes(
		attribute.Int("changeCount", len(t.changes.values)),
	))
	defer span.End()

	// Call this here instead of in [t.db.commitChanges]
	// because doing so there would be a deadlock.
	if err := t.calculateNodeIDs(ctx); err != nil {
		return err
	}

	if err := t.db.commitChanges(ctx, t); err != nil {
		return err
	}

	t.committed = true

	return nil
}

// Assumes [t.validityTrackingLock] isn't held.
func (t *trieView) isInvalid() bool {
	t.validityTrackingLock.RLock()
	defer t.validityTrackingLock.RUnlock()

	return t.invalidated
}

// Invalidates this view and all descendants.
// Assumes [t.validityTrackingLock] isn't held.
func (t *trieView) invalidate() {
	t.validityTrackingLock.Lock()
	defer t.validityTrackingLock.Unlock()

	t.invalidated = true

	for _, childView := range t.childViews {
		childView.invalidate()
	}

	// after invalidating the children, they no longer need to be tracked
	t.childViews = make([]*trieView, 0, defaultPreallocationSize)
}

func (t *trieView) updateParent(newParent TrieView) {
	t.validityTrackingLock.Lock()
	defer t.validityTrackingLock.Unlock()

	t.parentTrie = newParent
}

// GetMerkleRoot returns the ID of the root of this trie.
func (t *trieView) GetMerkleRoot(ctx context.Context) (ids.ID, error) {
	if err := t.calculateNodeIDs(ctx); err != nil {
		return ids.Empty, err
	}
	return t.root.id, nil
}

func (t *trieView) GetValues(_ context.Context, keys [][]byte) ([][]byte, []error) {
	results := make([][]byte, len(keys))
	valueErrors := make([]error, len(keys))

	for i, key := range keys {
		results[i], valueErrors[i] = t.getValueCopy(newPath(key))
	}
	return results, valueErrors
}

// GetValue returns the value for the given [key].
// Returns database.ErrNotFound if it doesn't exist.
func (t *trieView) GetValue(_ context.Context, key []byte) ([]byte, error) {
	return t.getValueCopy(newPath(key))
}

// getValueCopy returns a copy of the value for the given [key].
// Returns database.ErrNotFound if it doesn't exist.
func (t *trieView) getValueCopy(key path) ([]byte, error) {
	val, err := t.getValue(key)
	if err != nil {
		return nil, err
	}
	return slices.Clone(val), nil
}

func (t *trieView) getValue(key path) ([]byte, error) {
	if t.isInvalid() {
		return nil, ErrInvalid
	}

	if change, ok := t.changes.values[key]; ok {
		t.db.metrics.ViewValueCacheHit()
		if change.after.IsNothing() {
			return nil, database.ErrNotFound
		}
		return change.after.Value(), nil
	}
	t.db.metrics.ViewValueCacheMiss()

	// if we don't have local copy of the key, then grab a copy from the parent trie
	value, err := t.getParentTrie().getValue(key)
	if err != nil {
		return nil, err
	}

	// ensure no ancestor changes occurred during execution
	if t.isInvalid() {
		return nil, ErrInvalid
	}

	return value, nil
}

// Must not be called after [calculateNodeIDs] has returned.
func (t *trieView) remove(key path) error {
	if t.nodesAlreadyCalculated.Get() {
		return ErrNodesAlreadyCalculated
	}

	nodePath, err := t.getPathTo(key)
	if err != nil {
		return err
	}

	nodeToDelete := nodePath[len(nodePath)-1]

	if nodeToDelete.key.Compare(key) != 0 || !nodeToDelete.hasValue() {
		// the key wasn't in the trie or doesn't have a value so there's nothing to do
		return nil
	}

	// A node with ancestry [nodePath] is being deleted, so we need to recalculate
	// all the nodes in this path.
	for _, node := range nodePath {
		if err := t.recordNodeChange(node); err != nil {
			return err
		}
	}

	nodeToDelete.setValue(maybe.Nothing[[]byte]())
	if err := t.recordNodeChange(nodeToDelete); err != nil {
		return err
	}

	// if the removed node has no children, the node can be removed from the trie
	if len(nodeToDelete.children) == 0 {
		return t.deleteEmptyNodes(nodePath)
	}

	if len(nodePath) == 1 {
		return nil
	}
	parent := nodePath[len(nodePath)-2]

	// merge this node and its descendants into a single node if possible
	if err = t.compressNodePath(parent, nodeToDelete); err != nil {
		return err
	}

	return nil
}

// Merges together nodes in the inclusive descendants of [node] that
// have no value and a single child into one node with a compressed
// path until a node that doesn't meet those criteria is reached.
// [parent] is [node]'s parent.
// Assumes at least one of the following is true:
// * [node] has a value.
// * [node] has children.
// Must not be called after [calculateNodeIDs] has returned.
func (t *trieView) compressNodePath(parent, node *node) error {
	if t.nodesAlreadyCalculated.Get() {
		return ErrNodesAlreadyCalculated
	}

	// don't collapse into this node if it's the root, doesn't have 1 child, or has a value
	if len(node.children) != 1 || node.hasValue() {
		return nil
	}

	// delete all empty nodes with a single child under [node]
	for len(node.children) == 1 && !node.hasValue() {
		if err := t.recordNodeDeleted(node); err != nil {
			return err
		}

		nextNode, err := t.getNodeFromParent(node, node.getSingleChildPath())
		if err != nil {
			return err
		}
		node = nextNode
	}

	// [node] is the first node with multiple children.
	// combine it with the [node] passed in.
	parent.addChild(node)
	return t.recordNodeChange(parent)
}

// Starting from the last node in [nodePath], traverses toward the root
// and deletes each node that has no value and no children.
// Stops when a node with a value or children is reached.
// Assumes [nodePath] is a path from the root to a node.
// Must not be called after [calculateNodeIDs] has returned.
func (t *trieView) deleteEmptyNodes(nodePath []*node) error {
	if t.nodesAlreadyCalculated.Get() {
		return ErrNodesAlreadyCalculated
	}

	node := nodePath[len(nodePath)-1]
	nextParentIndex := len(nodePath) - 2

	for ; nextParentIndex >= 0 && len(node.children) == 0 && !node.hasValue(); nextParentIndex-- {
		if err := t.recordNodeDeleted(node); err != nil {
			return err
		}

		parent := nodePath[nextParentIndex]

		parent.removeChild(node)
		if err := t.recordNodeChange(parent); err != nil {
			return err
		}

		node = parent
	}

	if nextParentIndex < 0 {
		return nil
	}
	parent := nodePath[nextParentIndex]

	return t.compressNodePath(parent, node)
}

// Returns the nodes along the path to [key].
// The first node is the root, and the last node is either the node with the
// given [key], if it's in the trie, or the node with the largest prefix of
// the [key] if it isn't in the trie.
// Always returns at least the root node.
func (t *trieView) getPathTo(key path) ([]*node, error) {
	var (
		// all paths start at the root
		currentNode     = t.root
		matchedKeyIndex = 0
		nodes           = []*node{t.root}
	)

	// while the entire path hasn't been matched
	for matchedKeyIndex < len(key) {
		// confirm that a child exists and grab its ID before attempting to load it
		nextChildEntry, hasChild := currentNode.children[key[matchedKeyIndex]]

		// the nibble for the child entry has now been handled, so increment the matchedPathIndex
		matchedKeyIndex += 1

		if !hasChild || !key[matchedKeyIndex:].HasPrefix(nextChildEntry.compressedPath) {
			// there was no child along the path or the child that was there doesn't match the remaining path
			return nodes, nil
		}

		// the compressed path of the entry there matched the path, so increment the matched index
		matchedKeyIndex += len(nextChildEntry.compressedPath)

		// grab the next node along the path
		var err error
		currentNode, err = t.getNodeWithID(nextChildEntry.id, key[:matchedKeyIndex])
		if err != nil {
			return nil, err
		}

		// add node to path
		nodes = append(nodes, currentNode)
	}
	return nodes, nil
}

func getLengthOfCommonPrefix(first, second path) int {
	commonIndex := 0
	for len(first) > commonIndex && len(second) > commonIndex && first[commonIndex] == second[commonIndex] {
		commonIndex++
	}
	return commonIndex
}

// Get a copy of the node matching the passed key from the trie.
// Used by views to get nodes from their ancestors.
func (t *trieView) getEditableNode(key path) (*node, error) {
	if t.isInvalid() {
		return nil, ErrInvalid
	}

	// grab the node in question
	n, err := t.getNodeWithID(ids.Empty, key)
	if err != nil {
		return nil, err
	}

	// ensure no ancestor changes occurred during execution
	if t.isInvalid() {
		return nil, ErrInvalid
	}

	// return a clone of the node, so it can be edited without affecting this trie
	return n.clone(), nil
}

// insert a key/value pair into the correct node of the trie.
// Must not be called after [calculateNodeIDs] has returned.
func (t *trieView) insert(
	key path,
	value maybe.Maybe[[]byte],
) (*node, error) {
	if t.nodesAlreadyCalculated.Get() {
		return nil, ErrNodesAlreadyCalculated
	}

	// find the node that most closely matches [key]
	pathToNode, err := t.getPathTo(key)
	if err != nil {
		return nil, err
	}

	// We're inserting a node whose ancestry is [pathToNode]
	// so we'll need to recalculate their IDs.
	for _, node := range pathToNode {
		if err := t.recordNodeChange(node); err != nil {
			return nil, err
		}
	}

	closestNode := pathToNode[len(pathToNode)-1]

	// a node with that exact path already exists so update its value
	if closestNode.key.Compare(key) == 0 {
		closestNode.setValue(value)
		return closestNode, nil
	}

	closestNodeKeyLength := len(closestNode.key)
	// A node with the exact key doesn't exist so determine the portion of the
	// key that hasn't been matched yet
	// Note that [key] has prefix [closestNodeFullPath] but exactMatch was false,
	// so [key] must be longer than [closestNodeFullPath] and the following slice won't OOB.
	remainingKey := key[closestNodeKeyLength+1:]

	existingChildEntry, hasChild := closestNode.children[key[closestNodeKeyLength]]
	// there are no existing nodes along the path [fullPath], so create a new node to insert [value]
	if !hasChild {
		newNode := newNode(
			closestNode,
			key,
		)
		newNode.setValue(value)
		return newNode, t.recordNodeChange(newNode)
	}

	// if we have reached this point, then the [fullpath] we are trying to insert and
	// the existing path node have some common prefix.
	// a new branching node will be created that will represent this common prefix and
	// have the existing path node and the value being inserted as children.

	// generate the new branch node
	branchNode := newNode(
		closestNode,
		key[:closestNodeKeyLength+1+getLengthOfCommonPrefix(existingChildEntry.compressedPath, remainingKey)],
	)
	if err := t.recordNodeChange(closestNode); err != nil {
		return nil, err
	}
	nodeWithValue := branchNode

	if len(key)-len(branchNode.key) == 0 {
		// there was no residual path for the inserted key, so the value goes directly into the new branch node
		branchNode.setValue(value)
	} else {
		// generate a new node and add it as a child of the branch node
		newNode := newNode(
			branchNode,
			key,
		)
		newNode.setValue(value)
		if err := t.recordNodeChange(newNode); err != nil {
			return nil, err
		}
		nodeWithValue = newNode
	}

	existingChildKey := key[:closestNodeKeyLength+1] + existingChildEntry.compressedPath

	// the existing child's key is of length: len(closestNodeKey) + 1 for the child index + len(existing child's compressed key)
	// if that length is less than or equal to the branch node's key that implies that the existing child's key matched the key to be inserted
	// since it matched the key to be inserted, it should have been returned by GetPathTo
	if len(existingChildKey) <= len(branchNode.key) {
		return nil, ErrGetPathToFailure
	}

	branchNode.addChildWithoutNode(
		existingChildKey[len(branchNode.key)],
		existingChildKey[len(branchNode.key)+1:],
		existingChildEntry.id,
	)

	return nodeWithValue, t.recordNodeChange(branchNode)
}

// Records that a node has been changed.
// Must not be called after [calculateNodeIDs] has returned.
func (t *trieView) recordNodeChange(after *node) error {
	return t.recordKeyChange(after.key, after)
}

// Records that the node associated with the given key has been deleted.
// Must not be called after [calculateNodeIDs] has returned.
func (t *trieView) recordNodeDeleted(after *node) error {
	// don't delete the root.
	if len(after.key) == 0 {
		return t.recordKeyChange(after.key, after)
	}
	return t.recordKeyChange(after.key, nil)
}

// Records that the node associated with the given key has been changed.
// Must not be called after [calculateNodeIDs] has returned.
func (t *trieView) recordKeyChange(key path, after *node) error {
	if t.nodesAlreadyCalculated.Get() {
		return ErrNodesAlreadyCalculated
	}

	if existing, ok := t.changes.nodes[key]; ok {
		existing.after = after
		return nil
	}

	before, err := t.getParentTrie().getEditableNode(key)
	if err != nil {
		if err != database.ErrNotFound {
			return err
		}
		before = nil
	}

	t.changes.nodes[key] = &change[*node]{
		before: before,
		after:  after,
	}
	return nil
}

// Records that a key's value has been added or updated.
// Doesn't actually change the trie data structure.
// That's deferred until we call [calculateNodeIDs].
// Must not be called after [calculateNodeIDs] has returned.
func (t *trieView) recordValueChange(key path, value maybe.Maybe[[]byte]) error {
	if t.nodesAlreadyCalculated.Get() {
		return ErrNodesAlreadyCalculated
	}

	// update the existing change if it exists
	if existing, ok := t.changes.values[key]; ok {
		existing.after = value
		return nil
	}

	// grab the before value
	var beforeMaybe maybe.Maybe[[]byte]
	before, err := t.getParentTrie().getValue(key)
	switch err {
	case nil:
		beforeMaybe = maybe.Some(before)
	case database.ErrNotFound:
		beforeMaybe = maybe.Nothing[[]byte]()
	default:
		return err
	}

	t.changes.values[key] = &change[maybe.Maybe[[]byte]]{
		before: beforeMaybe,
		after:  value,
	}
	return nil
}

// Retrieves the node with the given [key], which is a child of [parent], and
// uses the [parent] node to initialize the child node's ID.
// Returns database.ErrNotFound if the child doesn't exist.
func (t *trieView) getNodeFromParent(parent *node, key path) (*node, error) {
	// confirm the child exists and get its ID before attempting to load it
	if child, exists := parent.children[key[len(parent.key)]]; exists {
		return t.getNodeWithID(child.id, key)
	}

	return nil, database.ErrNotFound
}

// Retrieves a node with the given [key].
// If the node is fetched from [t.parentTrie] and [id] isn't empty,
// sets the node's ID to [id].
// Returns database.ErrNotFound if the node doesn't exist.
func (t *trieView) getNodeWithID(id ids.ID, key path) (*node, error) {
	// check for the key within the changed nodes
	if nodeChange, isChanged := t.changes.nodes[key]; isChanged {
		t.db.metrics.ViewNodeCacheHit()
		if nodeChange.after == nil {
			return nil, database.ErrNotFound
		}
		return nodeChange.after, nil
	}

	// get the node from the parent trie and store a local copy
	parentTrieNode, err := t.getParentTrie().getEditableNode(key)
	if err != nil {
		return nil, err
	}

	// only need to initialize the id if it's from the parent trie.
	// nodes in the current view change list have already been initialized.
	if id != ids.Empty {
		parentTrieNode.id = id
	}
	return parentTrieNode, nil
}

// Get the parent trie of the view
func (t *trieView) getParentTrie() TrieView {
	t.validityTrackingLock.RLock()
	defer t.validityTrackingLock.RUnlock()
	return t.parentTrie
}
