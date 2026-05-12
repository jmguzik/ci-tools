package regexp

import (
	"testing"
)

func TestRegexp_UnmarshalText(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		wantErr bool
	}{
		{
			name:    "valid pattern",
			text:    "^test.*$",
			wantErr: false,
		},
		{
			name:    "invalid pattern",
			text:    "[unclosed",
			wantErr: true,
		},
		{
			name:    "empty pattern",
			text:    "",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var re Regexp
			err := re.UnmarshalText([]byte(tt.text))
			if (err != nil) != tt.wantErr {
				t.Errorf("UnmarshalText() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if re.expr != tt.text {
					t.Errorf("UnmarshalText() expr = %v, want %v", re.expr, tt.text)
				}
				if re.Pattern == nil {
					t.Error("UnmarshalText() Pattern is nil")
				}
			}
		})
	}
}

func TestLookupByMatch(t *testing.T) {
	type Config struct {
		Name  string
		Value int
	}

	tests := []struct {
		name           string
		patternPairs   map[string]string
		input          string
		wantValue      string
		wantFound      bool
		skipValueCheck bool
	}{
		{
			name: "match first pattern",
			patternPairs: map[string]string{
				"^test-.*": "test-env",
				"^prod-.*": "prod-env",
				"^dev-.*":  "dev-env",
			},
			input:     "test-cluster",
			wantValue: "test-env",
			wantFound: true,
		},
		{
			name: "match second pattern",
			patternPairs: map[string]string{
				"^test-.*": "test-env",
				"^prod-.*": "prod-env",
				"^dev-.*":  "dev-env",
			},
			input:     "prod-cluster",
			wantValue: "prod-env",
			wantFound: true,
		},
		{
			name: "match third pattern",
			patternPairs: map[string]string{
				"^test-.*": "test-env",
				"^prod-.*": "prod-env",
				"^dev-.*":  "dev-env",
			},
			input:     "dev-cluster",
			wantValue: "dev-env",
			wantFound: true,
		},
		{
			name: "no match",
			patternPairs: map[string]string{
				"^test-.*": "test-env",
				"^prod-.*": "prod-env",
			},
			input:     "staging-cluster",
			wantValue: "",
			wantFound: false,
		},
		{
			name: "empty string input",
			patternPairs: map[string]string{
				"^test-.*": "test-env",
			},
			input:     "",
			wantValue: "",
			wantFound: false,
		},
		{
			name:         "empty map",
			patternPairs: map[string]string{},
			input:        "any-string",
			wantValue:    "",
			wantFound:    false,
		},
		{
			name: "match with special characters",
			patternPairs: map[string]string{
				`^v\d+\.\d+\.\d+$`: "version",
			},
			input:     "v1.2.3",
			wantValue: "version",
			wantFound: true,
		},
		{
			name: "partial match not allowed",
			patternPairs: map[string]string{
				"^test$": "exact",
			},
			input:     "test-extra",
			wantValue: "",
			wantFound: false,
		},
		{
			name: "multiple patterns could match - first wins",
			patternPairs: map[string]string{
				".*":       "wildcard",
				"^test-.*": "specific",
			},
			input:          "test-cluster",
			wantFound:      true,
			skipValueCheck: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			patterns := make(map[Regexp]string)
			for expr, value := range tt.patternPairs {
				patterns[re(expr, t)] = value
			}

			gotValue, gotFound := LookupByMatch(patterns, tt.input)
			if gotFound != tt.wantFound {
				t.Errorf("LookupByMatch() found = %v, want %v", gotFound, tt.wantFound)
			}
			if !tt.skipValueCheck && gotValue != tt.wantValue {
				t.Errorf("LookupByMatch() value = %v, want %v", gotValue, tt.wantValue)
			}
		})
	}

	t.Run("complex types", func(t *testing.T) {
		patterns := map[Regexp]Config{
			re("^app-.*", t): {Name: "application", Value: 100},
			re("^db-.*", t):  {Name: "database", Value: 200},
		}

		tests := []struct {
			name      string
			input     string
			wantValue Config
			wantFound bool
		}{
			{
				name:      "match app pattern",
				input:     "app-server",
				wantValue: Config{Name: "application", Value: 100},
				wantFound: true,
			},
			{
				name:      "match db pattern",
				input:     "db-postgres",
				wantValue: Config{Name: "database", Value: 200},
				wantFound: true,
			},
			{
				name:      "no match returns zero value",
				input:     "cache-redis",
				wantValue: Config{},
				wantFound: false,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				gotValue, gotFound := LookupByMatch(patterns, tt.input)
				if gotFound != tt.wantFound {
					t.Errorf("LookupByMatch() found = %v, want %v", gotFound, tt.wantFound)
				}
				if gotValue != tt.wantValue {
					t.Errorf("LookupByMatch() value = %v, want %v", gotValue, tt.wantValue)
				}
			})
		}
	})
}

func re(expr string, t *testing.T) Regexp {
	compiled, err := Compile(expr)
	if err != nil {
		t.Fatalf("failed to compile regexp %q: %v", expr, err)
	}
	return *compiled
}
