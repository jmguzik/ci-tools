package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	prowpluginflagutil "sigs.k8s.io/prow/pkg/flagutil/plugins"

	"gopkg.in/yaml.v3"
)

const (
	defaultRequiredApp = "openshift-merge-bot"
)

type options struct {
	plugins     prowpluginflagutil.PluginOptions
	requiredApp string
}

type pluginConfig struct {
	Triggers []triggerConfig `yaml:"triggers"`
}

type triggerConfig struct {
	Repos       []string `yaml:"repos"`
	TrustedApps []string `yaml:"trusted_apps"`
}

func gatherOptions() options {
	o := options{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	o.plugins.PluginConfigPathDefault = ""
	o.plugins.SupplementalPluginsConfigsFileNameSuffix = "_pluginconfig.yaml"
	o.plugins.AddFlags(fs)
	fs.StringVar(&o.requiredApp, "required-trusted-app", defaultRequiredApp, "Trusted app that every trigger entry must include.")

	if err := fs.Parse(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "failed to parse arguments: %v\n", err)
		os.Exit(2)
	}

	return o
}

func (o options) validate() error {
	if o.plugins.PluginConfigPath == "" {
		return errors.New("--plugin-config is required")
	}
	if strings.TrimSpace(o.requiredApp) == "" {
		return errors.New("--required-trusted-app cannot be empty")
	}
	return nil
}

func main() {
	o := gatherOptions()
	if err := o.validate(); err != nil {
		fmt.Fprintf(os.Stderr, "invalid options: %v\n", err)
		os.Exit(2)
	}

	files, err := pluginConfigFiles(o.plugins.PluginConfigPath, o.plugins.SupplementalPluginsConfigDirs.Strings(), o.plugins.SupplementalPluginsConfigsFileNameSuffix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to discover plugin config files: %v\n", err)
		os.Exit(2)
	}

	var failures []string
	for _, file := range files {
		cfg, err := loadPluginConfig(file)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: failed to parse file: %v", file, err))
			continue
		}
		failures = append(failures, validateTriggerConfig(file, cfg, o.requiredApp)...)
	}

	if len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "trigger trusted-apps validation failed (%d issue(s)):\n", len(failures))
		for _, f := range failures {
			fmt.Fprintf(os.Stderr, "  - %s\n", f)
		}
		os.Exit(1)
	}

	fmt.Printf("validated %d plugin config file(s): every trigger has trusted_apps including %q\n", len(files), o.requiredApp)
}

func pluginConfigFiles(baseConfig string, supplementalDirs []string, suffix string) ([]string, error) {
	files := map[string]struct{}{baseConfig: {}}

	for _, dir := range supplementalDirs {
		if dir == "" {
			continue
		}
		if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if strings.HasSuffix(path, suffix) {
				files[path] = struct{}{}
			}
			return nil
		}); err != nil {
			return nil, fmt.Errorf("walk %q: %w", dir, err)
		}
	}

	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	return paths, nil
}

func loadPluginConfig(path string) (*pluginConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &pluginConfig{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func validateTriggerConfig(file string, cfg *pluginConfig, requiredApp string) []string {
	var failures []string
	for i, tr := range cfg.Triggers {
		repoScope := "<none>"
		if len(tr.Repos) > 0 {
			repoScope = strings.Join(tr.Repos, ",")
		}
		if len(tr.TrustedApps) == 0 {
			failures = append(failures, fmt.Sprintf("%s: triggers[%d] (repos=%s) has no trusted_apps", file, i, repoScope))
			continue
		}
		if !slices.Contains(tr.TrustedApps, requiredApp) {
			failures = append(failures, fmt.Sprintf("%s: triggers[%d] (repos=%s) missing trusted app %q", file, i, repoScope, requiredApp))
		}
	}
	return failures
}
