package revlist

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/storage/memory"
)

type countedObjects struct {
	storer.EncodedObjectStorer
	reads, sizes int
}

func (s *countedObjects) EncodedObject(kind plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	s.reads++
	return s.EncodedObjectStorer.EncodedObject(kind, h)
}

func (s *countedObjects) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	s.sizes++
	return s.EncodedObjectStorer.EncodedObjectSize(h)
}

func TestObjectsOptions(t *testing.T) {
	t.Parallel()
	st := memory.NewStorage()
	blob := st.NewEncodedObject()
	blob.SetType(plumbing.BlobObject)
	w, err := blob.Writer()
	require.NoError(t, err)
	_, err = w.Write([]byte("blob"))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	hash, err := st.SetEncodedObject(blob)
	require.NoError(t, err)
	tree := testMakeTree(t, st, []object.TreeEntry{{Name: "blob", Mode: filemode.Regular, Hash: hash}})
	tip := testMakeCommitAt(t, st, tree, time.Unix(1, 0))
	tagObject := st.NewEncodedObject()
	require.NoError(t, (&object.Tag{Name: "tag", Target: hash, TargetType: plumbing.BlobObject}).Encode(tagObject))
	tag, err := st.SetEncodedObject(tagObject)
	require.NoError(t, err)
	zero, limit := uint64(0), uint64(4)
	for _, tc := range []struct {
		name                string
		opts                ObjectsOptions
		count, reads, sizes int
	}{
		{name: "ordinary", count: 3, reads: 2},
		{name: "include-wants", opts: ObjectsOptions{IncludeWants: true}, count: 3, reads: 2},
		{name: "blob-none", opts: ObjectsOptions{BlobLimit: &zero, IncludeWants: true}, count: 2, reads: 2},
		{name: "blob-limit", opts: ObjectsOptions{BlobLimit: &limit, IncludeWants: true}, count: 2, reads: 2, sizes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			counted := &countedObjects{EncodedObjectStorer: st}
			got, err := ObjectsWithOptions(counted, []plumbing.Hash{tip}, nil, tc.opts)
			require.NoError(t, err)
			require.Len(t, got, tc.count)
			require.Equal(t, tc.reads, counted.reads)
			require.Equal(t, tc.sizes, counted.sizes)
		})
	}
	for _, wants := range [][]plumbing.Hash{{tree, hash}, {hash, tree}, {tree, tag}, {tag, tree}} {
		for _, filter := range []*uint64{nil, &zero, &limit} {
			got, err := ObjectsWithOptions(st, wants, nil, ObjectsOptions{BlobLimit: filter, IncludeWants: true})
			require.NoError(t, err)
			expected := []plumbing.Hash{tree, hash}
			if wants[0] == tag || wants[1] == tag {
				expected = append(expected, tag)
			}
			require.ElementsMatch(t, expected, got)
			if filter != nil {
				got, err = ObjectsWithOptions(st, wants, nil, ObjectsOptions{BlobLimit: filter})
				require.NoError(t, err)
				require.ElementsMatch(t, expected, got)
			}
		}
	}
	for _, want := range []plumbing.Hash{hash, tag} {
		got, err := ObjectsWithOptions(st, []plumbing.Hash{want}, []plumbing.Hash{tip}, ObjectsOptions{BlobLimit: &zero, IncludeWants: true})
		require.NoError(t, err)
		require.Contains(t, got, want)
		if want == tag {
			require.Contains(t, got, hash)
		}
	}
	got, err := ObjectsWithOptions(st, []plumbing.Hash{tip, hash}, []plumbing.Hash{tip}, ObjectsOptions{IncludeWants: true})
	require.NoError(t, err)
	require.Equal(t, []plumbing.Hash{hash}, got)
}

func TestObjectsOptionsZeroUsesSpecializedWalker(t *testing.T) {
	t.Parallel()
	want := plumbing.NewHash("1111111111111111111111111111111111111111")
	st := &delegatedObjectStorer{result: []plumbing.Hash{want}}
	got, err := ObjectsWithOptions(st, []plumbing.Hash{want}, nil, ObjectsOptions{})
	require.NoError(t, err)
	require.True(t, st.called)
	require.Equal(t, st.result, got)
}
