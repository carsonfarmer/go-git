package transport

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/revlist"
	"github.com/go-git/go-git/v6/storage"
)

// parseFetchFilter returns the exclusive blob size limit, or nil for no filter.
// Restrict serving to blob filters: tree and combined filters need different
// traversal semantics and must not silently produce an unfiltered response.
func parseFetchFilter(filter packp.Filter) (*uint64, error) {
	if filter == "" {
		return nil, nil
	}
	var limit uint64
	if filter == "blob:none" {
		return &limit, nil
	}
	value, ok := strings.CutPrefix(string(filter), "blob:limit=")
	if !ok || value == "" {
		return nil, fmt.Errorf("unsupported fetch filter: %q", filter)
	}
	var shift uint
	switch value[len(value)-1] {
	case 'k', 'K':
		shift = 10
	case 'm', 'M':
		shift = 20
	case 'g', 'G':
		shift = 30
	}
	if shift != 0 {
		value = value[:len(value)-1]
	}
	// Match Git's strtoumax base-zero syntax, without Go-only prefixes or separators.
	value = strings.TrimPrefix(strings.TrimLeft(value, " \t\n\r\v\f"), "+")
	base := 10
	if strings.HasPrefix(value, "0x") || strings.HasPrefix(value, "0X") {
		base, value = 16, value[2:]
	} else if strings.HasPrefix(value, "0") {
		base = 8
	}
	limit, err := strconv.ParseUint(value, base, 64)
	if err != nil || limit > math.MaxUint64>>shift {
		return nil, fmt.Errorf("invalid fetch filter: %q", filter)
	}
	limit <<= shift
	return &limit, nil
}

// explicitFetchWants keeps tag chains and filtered explicit tree closures.
// It never traverses commit history. Shallow subtraction and specialized
// walkers must not remove these objects based on common commit haves.
func explicitFetchWants(st storage.Storer, wants []plumbing.Hash, blobLimit *uint64) ([]plumbing.Hash, error) {
	wants = append([]plumbing.Hash(nil), wants...)
	seen := make(map[plumbing.Hash]struct{}, len(wants))
	var trees []plumbing.Hash
	for i := 0; i < len(wants); i++ {
		h := wants[i]
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		obj, err := st.EncodedObject(plumbing.AnyObject, h)
		if err != nil {
			return nil, err
		}
		switch obj.Type() {
		case plumbing.TreeObject:
			trees = append(trees, h)
		case plumbing.TagObject:
			tag, err := object.DecodeTag(st, obj)
			if err != nil {
				return nil, err
			}
			wants = append(wants, tag.Target)
		}
	}
	if len(trees) > 0 {
		objects, err := revlist.ObjectsWithOptions(st, trees, nil, revlist.ObjectsOptions{BlobLimit: blobLimit})
		if err != nil {
			return nil, err
		}
		wants = append(wants, objects...)
	}
	return wants, nil
}

// fetchObjects preserves specialized unfiltered walks. Only explicit objects
// need a supplemental selection, so ordinary commit fetches do not walk their
// history twice. Filtered and generic fetches select in the existing walk.
func fetchObjects(st storage.Storer, wants, haves []plumbing.Hash, selection revlist.ObjectsOptions) ([]plumbing.Hash, error) {
	if selection.BlobLimit != nil {
		return revlist.ObjectsWithOptions(st, wants, haves, selection)
	}
	if _, ok := st.(interface {
		RevListObjects(wants, haves []plumbing.Hash) ([]plumbing.Hash, error)
	}); !ok {
		return revlist.ObjectsWithOptions(st, wants, haves, selection)
	}
	objects, err := objectsToUpload(st, wants, haves)
	if err != nil {
		return nil, err
	}
	explicit, err := explicitFetchWants(st, wants, nil)
	if err != nil {
		return nil, err
	}
	missing := make(map[plumbing.Hash]struct{}, len(explicit))
	for _, h := range explicit {
		missing[h] = struct{}{}
	}
	for _, h := range objects {
		delete(missing, h)
	}
	objects = slices.Clip(objects)
	for _, h := range explicit {
		if _, ok := missing[h]; ok {
			objects = append(objects, h)
			delete(missing, h)
		}
	}
	return objects, nil
}
