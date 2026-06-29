package economy_test

import (
	"context"
	"testing"

	"github.com/hash066/cerberus/daemon/economy"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
)

func TestFetchWeight(t *testing.T) {
	// Create a dummy CID
	mh, err := multihash.Encode([]byte("dummy"), multihash.SHA2_256)
	if err != nil {
		t.Fatal(err)
	}
	c := cid.NewCidV1(cid.Raw, mh)

	data, err := economy.FetchWeight(context.Background(), c, 2, 2)
	if err != nil {
		t.Fatalf("FetchWeight failed: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("Expected non-empty data")
	}
}
