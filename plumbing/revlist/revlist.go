// Package revlist provides support to access the ancestors of commits, in a
// similar way as the git-rev-list command.
package revlist

import (
	"slices"

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
	// IncludeWants keeps explicitly requested non-commit objects, including
	// annotated tag targets and filtered tree descendants, even when
	// reachable from haves (for promisor clients).
	IncludeWants bool
}

// ObjectsWithOptions applies selection during the object walk. A storer's
// specialized walker is used unless BlobLimit is set; IncludeWants then
// supplements its result with the explicitly wanted objects it omitted.
func ObjectsWithOptions(
	s storer.EncodedObjectStorer,
	wants, haves []plumbing.Hash,
	opts ObjectsOptions,
) ([]plumbing.Hash, error) {
	if walker, ok := s.(objectWalker); ok && opts.BlobLimit == nil {
		objs, err := walker.RevListObjects(wants, haves)
		if err != nil || !opts.IncludeWants {
			return objs, err
		}
		explicit, err := ExplicitObjects(s, wants, nil)
		if err != nil {
			return nil, err
		}
		return appendMissing(objs, explicit), nil
	}
	w, err := newObjectWalk(s)
	if err != nil {
		return nil, err
	}
	w.blobLimit = opts.BlobLimit
	w.includeWants = opts.IncludeWants
	if err := w.seedHaves(haves); err != nil {
		return nil, err
	}
	if err := w.seedWants(wants); err != nil {
		return nil, err
	}
	if err := w.walk(); err != nil {
		return nil, err
	}
	return w.result, nil
}

// ExplicitObjects returns the objects wants name directly: blobs, tags with
// their peeled chain, and the closure of trees, filtered by blobLimit as in
// ObjectsOptions. Commit wants contribute nothing; history is never walked.
func ExplicitObjects(s storer.EncodedObjectStorer, wants []plumbing.Hash, blobLimit *uint64) ([]plumbing.Hash, error) {
	w := &objectWalk{
		s:            s,
		blobLimit:    blobLimit,
		includeWants: true,
		explicitOnly: true,
		wantsSeen:    make(map[plumbing.Hash]struct{}, len(wants)),
		seen:         make(map[plumbing.Hash]struct{}),
	}
	if err := w.seedWants(wants); err != nil {
		return nil, err
	}
	return w.result, nil
}

// appendMissing appends the elements of extra absent from objs, tracking only
// extra so a large objs is not indexed.
func appendMissing(objs, extra []plumbing.Hash) []plumbing.Hash {
	if len(extra) == 0 {
		return objs
	}
	missing := make(map[plumbing.Hash]struct{}, len(extra))
	for _, h := range extra {
		missing[h] = struct{}{}
	}
	for _, h := range objs {
		delete(missing, h)
	}
	objs = slices.Clip(objs)
	for _, h := range extra {
		if _, ok := missing[h]; ok {
			objs = append(objs, h)
			delete(missing, h)
		}
	}
	return objs
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
