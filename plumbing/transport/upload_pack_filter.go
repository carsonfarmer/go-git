package transport

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
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
