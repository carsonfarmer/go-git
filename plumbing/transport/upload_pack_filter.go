package transport

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
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
	// ParseUint accepts a leading '+', which is not part of this wire grammar.
	if value == "" || value[0] < '0' || value[0] > '9' {
		return nil, fmt.Errorf("invalid fetch filter: %q", filter)
	}
	limit, err := strconv.ParseUint(value, 10, 64)
	if err != nil || limit > math.MaxUint64>>shift {
		return nil, fmt.Errorf("invalid fetch filter: %q", filter)
	}
	limit <<= shift
	return &limit, nil
}

// explicitFetchWants expands only requested tag chains, not the object graph.
// Shallow set subtraction must not remove these objects based on common haves.
func explicitFetchWants(st storage.Storer, wants []plumbing.Hash) ([]plumbing.Hash, error) {
	wants = append([]plumbing.Hash(nil), wants...)
	seen := make(map[plumbing.Hash]struct{}, len(wants))
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
		if obj.Type() == plumbing.TagObject {
			tag, err := object.DecodeTag(st, obj)
			if err != nil {
				return nil, err
			}
			wants = append(wants, tag.Target)
		}
	}
	return wants, nil
}
