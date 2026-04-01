package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateTriggerConfig(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		cfg          pluginConfig
		requiredApp  string
		wantFailures int
	}{
		{
			name: "all good",
			cfg: pluginConfig{
				Triggers: []triggerConfig{
					{Repos: []string{"org/repo"}, TrustedApps: []string{"openshift-merge-bot", "dependabot"}},
				},
			},
			requiredApp:  "openshift-merge-bot",
			wantFailures: 0,
		},
		{
			name: "missing trusted apps list",
			cfg: pluginConfig{
				Triggers: []triggerConfig{
					{Repos: []string{"org/repo"}},
				},
			},
			requiredApp:  "openshift-merge-bot",
			wantFailures: 1,
		},
		{
			name: "missing required app",
			cfg: pluginConfig{
				Triggers: []triggerConfig{
					{Repos: []string{"org/repo"}, TrustedApps: []string{"dependabot"}},
				},
			},
			requiredApp:  "openshift-merge-bot",
			wantFailures: 1,
		},
		{
			name: "multiple triggers with two failures",
			cfg: pluginConfig{
				Triggers: []triggerConfig{
					{Repos: []string{"org/repo1"}, TrustedApps: []string{"openshift-merge-bot"}},
					{Repos: []string{"org/repo2"}, TrustedApps: []string{"dependabot"}},
					{Repos: []string{"org/repo3"}},
				},
			},
			requiredApp:  "openshift-merge-bot",
			wantFailures: 2,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			failures := validateTriggerConfig("test.yaml", &tc.cfg, tc.requiredApp)
			if len(failures) != tc.wantFailures {
				t.Fatalf("expected %d failures, got %d: %v", tc.wantFailures, len(failures), failures)
			}
		})
	}
}

func TestPluginConfigFiles(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	base := filepath.Join(tmp, "_plugins.yaml")
	if err := os.WriteFile(base, []byte("plugins: {}\n"), 0o644); err != nil {
		t.Fatalf("write base: %v", err)
	}

	sub := filepath.Join(tmp, "org", "repo")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	good := filepath.Join(sub, "_pluginconfig.yaml")
	other := filepath.Join(sub, "other.yaml")
	if err := os.WriteFile(good, []byte("plugins: {}\n"), 0o644); err != nil {
		t.Fatalf("write supplemental: %v", err)
	}
	if err := os.WriteFile(other, []byte("plugins: {}\n"), 0o644); err != nil {
		t.Fatalf("write non-supplemental: %v", err)
	}

	files, err := pluginConfigFiles(base, []string{tmp}, "_pluginconfig.yaml")
	if err != nil {
		t.Fatalf("pluginConfigFiles returned error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files (base + one supplemental), got %d: %v", len(files), files)
	}
	if files[0] != base && files[1] != base {
		t.Fatalf("expected base config %q in file list: %v", base, files)
	}
	if files[0] != good && files[1] != good {
		t.Fatalf("expected supplemental config %q in file list: %v", good, files)
	}
}
