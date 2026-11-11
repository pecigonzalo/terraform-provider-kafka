package provider

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestJoinErrors(t *testing.T) {
	errBoom := errors.New("boom")
	errOther := errors.New("other")

	tests := []struct {
		name string
		errs map[string]error
		// want is the expected error message; empty means no error.
		want string
	}{
		{
			name: "empty_map_returns_nil",
			errs: map[string]error{},
			want: "",
		},
		{
			name: "all_nil_returns_nil",
			errs: map[string]error{"orders": nil, "users": nil},
			want: "",
		},
		{
			name: "one_error",
			errs: map[string]error{"orders": errBoom},
			want: "orders: boom",
		},
		{
			name: "several_errors_sorted_by_key",
			errs: map[string]error{"zeta": errOther, "alpha": errBoom},
			want: "alpha: boom\nzeta: other",
		},
		{
			name: "nil_entry_before_error_is_skipped",
			errs: map[string]error{"a": nil, "b": errBoom},
			want: "b: boom",
		},
	}

	describe := func(name string) string { return name }
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := joinErrors(tc.errs, describe)
			if tc.want == "" {
				assert.NoError(t, got)
				return
			}
			if assert.Error(t, got) {
				assert.Equal(t, tc.want, got.Error())
			}
		})
	}
}

func TestJoinErrorsKeepsWrappedCause(t *testing.T) {
	errBoom := errors.New("boom")

	got := joinErrors(map[string]error{"orders": errBoom}, func(name string) string { return name })

	assert.ErrorIs(t, got, errBoom)
}
