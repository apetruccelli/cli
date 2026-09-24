package registry

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseOwnerRef(t *testing.T) {
	tests := []struct {
		name    string
		member  string
		want    map[string]any
		wantErr string
	}{
		{
			name:   "user by id",
			member: "user:rkLvGn_STT6gEhsdo6kRdg",
			want:   map[string]any{"type": "USER", "id": "rkLvGn_STT6gEhsdo6kRdg"},
		},
		{
			// An email is accepted syntactically but sent as an id, which the API
			// will reject. Owners are addressed by id only; use "harness list user"
			// to resolve a person to their id.
			name:   "email is passed through as id",
			member: "user:alice@example.com",
			want:   map[string]any{"type": "USER", "id": "alice@example.com"},
		},
		{
			name:   "group",
			member: "group:_project_all_users",
			want:   map[string]any{"type": "GROUP", "identifier": "_project_all_users"},
		},
		{
			name:   "uppercase prefix",
			member: "USER:u1",
			want:   map[string]any{"type": "USER", "id": "u1"},
		},
		{
			name:    "missing prefix",
			member:  "u1",
			wantErr: `must be "user:<id>" or "group:<identifier>"`,
		},
		{
			name:    "unknown prefix",
			member:  "team:platform",
			wantErr: `must be prefixed with "user:" or "group:"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseOwnerRef(tt.member)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseOwnerRef(%q) error = %v, want error containing %q", tt.member, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOwnerRef(%q): %v", tt.member, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseOwnerRef(%q) = %v, want %v", tt.member, got, tt.want)
			}
		})
	}
}

func TestOwnerRefEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b map[string]any
		want bool
	}{
		{
			// The read shape carries extra fields (name); matching keys on id only.
			name: "same user id with extra read-only fields",
			a:    map[string]any{"type": "USER", "id": "u1", "name": "alice"},
			b:    map[string]any{"type": "USER", "id": "u1"},
			want: true,
		},
		{
			name: "different user ids",
			a:    map[string]any{"type": "USER", "id": "u1"},
			b:    map[string]any{"type": "USER", "id": "u2"},
			want: false,
		},
		{
			name: "same group identifier",
			a:    map[string]any{"type": "GROUP", "identifier": "g1"},
			b:    map[string]any{"type": "GROUP", "identifier": "g1"},
			want: true,
		},
		{
			name: "different group identifiers",
			a:    map[string]any{"type": "GROUP", "identifier": "g1"},
			b:    map[string]any{"type": "GROUP", "identifier": "g2"},
			want: false,
		},
		{
			name: "type mismatch",
			a:    map[string]any{"type": "USER", "id": "x"},
			b:    map[string]any{"type": "GROUP", "identifier": "x"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ownerRefEqual(tt.a, tt.b); got != tt.want {
				t.Errorf("ownerRefEqual(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
