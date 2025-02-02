// Copyright (C) 2019-2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package e2e

import (
	ginkgo "github.com/onsi/ginkgo/v2"
)

// DescribeXChain annotates the tests for X-Chain.
// Can run with any type of cluster (e.g., local, fuji, mainnet).
func DescribeXChain(text string, body func()) bool {
	return ginkgo.Describe("[X-Chain] "+text, body)
}

// DescribeXChainSerial annotates serial tests for X-Chain.
// Can run with any type of cluster (e.g., local, fuji, mainnet).
func DescribeXChainSerial(text string, body func()) bool {
	return ginkgo.Describe("[X-Chain] "+text, ginkgo.Serial, body)
}

// DescribePChain annotates the tests for P-Chain.
// Can run with any type of cluster (e.g., local, fuji, mainnet).
func DescribePChain(text string, body func()) bool {
	return ginkgo.Describe("[P-Chain] "+text, body)
}
