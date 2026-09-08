// Package revlist provides support to access the ancestors of commits, in a
// similar way as the git-rev-list command.
package revlist

import (
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

// objectWalker can be implemented by storers that provide a specialized
// revlist object walk for a wants/haves query.
type objectWalker interface {
	RevListObjects(wants, haves []plumbing.Hash) ([]plumbing.Hash, error)
}

// Objects computes object hashes reachable from wants while excluding
// commits reachable from haves.
//
// If s implements objectWalker, its RevListObjects method is used.
// Otherwise, Objects expands haves first to establish commit boundaries,
// then walks wants in the same object store.
func Objects(
	s storer.EncodedObjectStorer,
	wants,
	haves []plumbing.Hash,
) ([]plumbing.Hash, error) {
	return ObjectsWithOptions(s, wants, haves, ObjectsOptions{})
}

// ObjectsOptions controls object selection without changing the commit walk.
type ObjectsOptions struct {
	// BlobLimit omits indirect blobs of this size or larger. Nil keeps all
	// blobs; zero omits them without opening their metadata or contents.
	BlobLimit *uint64
	// IncludeWants keeps explicitly requested objects, including annotated
	// tag targets and filtered tree descendants, even when reachable from
	// haves (for promisor clients).
	IncludeWants bool
}

// ObjectsWithOptions applies selection during the built-in object walk.
// Unlike Objects, nonzero options do not use a storer's specialized walker.
// Zero options preserve Objects behavior, including its specialized walker.
func ObjectsWithOptions(
	s storer.EncodedObjectStorer,
	wants, haves []plumbing.Hash,
	opts ObjectsOptions,
) ([]plumbing.Hash, error) {
	if opts.BlobLimit == nil && !opts.IncludeWants {
		if walker, ok := s.(objectWalker); ok {
			return walker.RevListObjects(wants, haves)
		}
	}
	w, err := newObjectWalk(s)
	if err != nil {
		return nil, err
	}
	w.blobLimit = opts.BlobLimit
	if opts.IncludeWants {
		w.explicit = make(map[plumbing.Hash]bool, len(wants))
	}
	if err := w.seedHaves(haves); err != nil {
		return nil, err
	}
	if err := w.seedWants(wants); err != nil {
		return nil, err
	}
	if err := w.walk(); err != nil {
		return nil, err
	}
	for h, included := range w.explicit {
		if !included {
			w.result = append(w.result, h)
		}
	}
	return w.result, nil
}

// ObjectsWithRef returns a map from each reachable object hash to the
// list of want hashes that can reach it.
func ObjectsWithRef(
	s storer.EncodedObjectStorer,
	wants,
	haves []plumbing.Hash,
) (map[plumbing.Hash][]plumbing.Hash, error) {
	all := map[plumbing.Hash][]plumbing.Hash{}
	for _, want := range wants {
		hashes, err := Objects(s, []plumbing.Hash{want}, haves)
		if err != nil {
			return nil, err
		}
		for _, h := range hashes {
			all[h] = append(all[h], want)
		}
	}
	return all, nil
}
