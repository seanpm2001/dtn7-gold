package storage

import (
	"crypto/sha1"
	"fmt"
	"os"
	"path"
	"time"

	"github.com/dtn7/dtn7-go/bundle"
)

// BundleItem is a wrapper for meta data around a Bundle. The Store operates
// on BundleItems instead of Bundles.
type BundleItem struct {
	Id  string `badgerhold:"key"`
	BId bundle.BundleID

	Pending bool      `badgerholdIndex:"Pending"`
	Expires time.Time `badgerholdIndex:"Expires"`

	Fragmented bool
	Parts      []BundlePart

	Properties map[string]interface{}
}

// BundlePart links a BundleItem to a Bundle with possible information
// regarding fragmentations.
type BundlePart struct {
	Filename string

	FragmentOffset  uint64
	TotalDataLength uint64
}

// storeBundle serializes the Bundle of a BundleItem/BundlePart to the disk.
func (bp BundlePart) storeBundle(b bundle.Bundle) error {
	if f, err := os.OpenFile(bp.Filename, os.O_WRONLY|os.O_CREATE, 0600); err != nil {
		return err
	} else {
		return b.WriteBundle(f)
	}
}

// Load the Bundle struct from the disk.
func (bp BundlePart) Load() (b bundle.Bundle, err error) {
	if f, fErr := os.Open(bp.Filename); fErr != nil {
		err = fErr
	} else {
		b, err = bundle.ParseBundle(f)
	}
	return
}

// calcExpirationDate for a Bundle.
func calcExpirationDate(b bundle.Bundle) time.Time {
	// TODO: check Bundle Age Block
	return b.PrimaryBlock.CreationTimestamp.DtnTime().Time().Add(
		time.Duration(b.PrimaryBlock.Lifetime) * time.Microsecond)
}

// bundlePartPath returns a path for a Bundle.
func bundlePartPath(id bundle.BundleID, storagePath string) string {
	f := fmt.Sprintf("%x", sha1.Sum([]byte(id.String())))
	return path.Join(storagePath, f)
}

// newBundleItem creates a new BundleItem for a Bundle.
func newBundleItem(b bundle.Bundle, storagePath string) (bi BundleItem) {
	bid := b.ID()

	bi = BundleItem{
		Id:  bid.Scrub().String(),
		BId: bid.Scrub(),

		Pending: false,
		Expires: calcExpirationDate(b),

		Fragmented: b.PrimaryBlock.HasFragmentation(),

		Properties: make(map[string]interface{}),
	}

	bp := BundlePart{
		Filename: bundlePartPath(bid, storagePath),

		FragmentOffset:  bid.FragmentOffset,
		TotalDataLength: bid.TotalDataLength,
	}

	bi.Parts = append(bi.Parts, bp)

	return
}
