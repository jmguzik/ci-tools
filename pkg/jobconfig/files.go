package jobconfig

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/getlantern/deepcopy"
	"github.com/ghodss/yaml"
	"github.com/sirupsen/logrus"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/sets"
	prowconfig "sigs.k8s.io/prow/pkg/config"

	cioperatorapi "github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/util"
	"github.com/openshift/ci-tools/pkg/util/gzip"
)

type label string
type Generator string

const (
	CanBeRehearsedLabel          = "pj-rehearse.openshift.io/can-be-rehearsed"
	CanBeRehearsedValue          = "true"
	SSHBastionLabel              = "dptp.openshift.io/ssh-bastion"
	ProwJobLabelVariant          = "ci-operator.openshift.io/variant"
	ReleaseControllerLabel       = "ci-operator.openshift.io/release-controller"
	LabelBuildFarm               = "ci.openshift.io/build-farm"
	LabelGenerator               = "ci.openshift.io/generator"
	ReleaseControllerValue       = "true"
	JobReleaseKey                = "job-release"
	PresubmitPrefix              = "pull"
	PostsubmitPrefix             = "branch"
	PeriodicPrefix               = "periodic"
	newlyGenerated         label = "newly-generated"
)

// SimpleBranchRegexp matches a branch name that does not appear to be a regex (lacks wildcard,
// group, or other modifiers). For instance, `master` is considered simple, `master-.*` would
// not.
var SimpleBranchRegexp = regexp.MustCompile(`^[\w\-.]+$`)

// Info describes the metadata for a Prow job configuration file
type Info struct {
	Org    string
	Repo   string
	Branch string
	// Type is the type of ProwJob contained in this file
	Type string
	// Filename is the full path to the file on disk
	Filename string
}

// Basename returns the unique name for this file in the config
func (i *Info) Basename() string {
	parts := []string{i.Org, i.Repo, i.Branch, i.Type}
	if i.Type == "periodics" && i.Branch == "" {
		parts = []string{i.Org, i.Repo, i.Type}
	}
	return fmt.Sprintf("%s.yaml", strings.Join(parts, "-"))
}

// ConfigMapName returns the configmap in which we expect this file to be uploaded
func (i *Info) ConfigMapName() string {
	// put periodics not directly correlated to code in the misc job
	if i.Type == "periodics" && i.Branch == "" {
		return fmt.Sprintf("job-config-%s", cioperatorapi.FlavorForBranch(""))
	}
	flavor := cioperatorapi.FlavorForBranch(i.Branch)
	if flavor == "master" || flavor == "main" {
		return fmt.Sprintf("job-config-%s-%s", flavor, i.Type)
	}

	return fmt.Sprintf("job-config-%s", flavor)
}

// We use the directory/file naming convention to encode useful information
// about component repository information.
// The convention for prow job config files in this repo:
// ci-operator/jobs/ORGANIZATION/COMPONENT/ORGANIZATION-COMPONENT-BRANCH-JOBTYPE.yaml
func extractInfoFromPath(configFilePath string) (*Info, error) {
	configSpecDir := filepath.Dir(configFilePath)
	repo := filepath.Base(configSpecDir)
	if repo == "." || repo == "/" {
		return nil, fmt.Errorf("could not extract repo from '%s'", configFilePath)
	}

	org := filepath.Base(filepath.Dir(configSpecDir))
	if org == "." || org == "/" {
		return nil, fmt.Errorf("could not extract org from '%s'", configFilePath)
	}

	// take org/repo/org-repo-branch-type.yaml and:
	// consider only the base name, then
	// remove .yaml extension, then
	// strip the "org-repo-" prefix, then
	// isolate the "-type" suffix, then
	// extract the branch
	basename := filepath.Base(configFilePath)
	basenameWithoutSuffix := strings.TrimSuffix(basename, filepath.Ext(configFilePath))
	orgRepo := fmt.Sprintf("%s-%s-", org, repo)
	if !strings.HasPrefix(basenameWithoutSuffix, orgRepo) {
		return nil, fmt.Errorf("file name was not prefixed with %q: %q", orgRepo, basenameWithoutSuffix)
	}
	branchType := strings.TrimPrefix(basenameWithoutSuffix, orgRepo)
	typeIndex := strings.LastIndex(branchType, "-")
	var branch, jobType string
	if typeIndex == -1 {
		if branchType != "periodics" {
			return nil, fmt.Errorf("file name does not contain job type: %q", basenameWithoutSuffix)
		}
		branch = ""
		jobType = "periodics"
	} else {
		branch = branchType[:typeIndex]
		jobType = branchType[typeIndex+1:]
	}

	return &Info{
		Org:      org,
		Repo:     repo,
		Branch:   branch,
		Type:     jobType,
		Filename: configFilePath,
	}, nil
}

func OperateOnJobConfigDir(configDir string, knownInfraJobFiles sets.Set[string], callback func(*prowconfig.JobConfig, *Info) error) error {
	return OperateOnJobConfigSubdir(configDir, "", knownInfraJobFiles, callback)
}

func OperateOnJobConfigSubdir(configDir, subDir string, knownInfraJobFiles sets.Set[string], callback func(*prowconfig.JobConfig, *Info) error) error {
	inputCh := make(chan *Info)
	produce := func() error {
		defer close(inputCh)
		return OperateOnJobConfigSubdirPaths(configDir, subDir, knownInfraJobFiles, func(info *Info) error {
			inputCh <- info
			return nil
		})
	}
	type item struct {
		config *prowconfig.JobConfig
		info   *Info
	}
	outputCh := make(chan item)
	map_ := func() error {
		for info := range inputCh {
			configPart, err := readFromFile(info.Filename)
			if err != nil {
				logrus.WithField("source-file", info.Filename).WithError(err).Error("Failed to read Prow job config")
				continue
			}
			outputCh <- item{configPart, info}
		}
		return nil
	}
	errCh := make(chan error)
	reduce := func() error {
		for i := range outputCh {
			if err := callback(i.config, i.info); err != nil {
				errCh <- err
			}
		}
		return nil
	}
	done := func() { close(outputCh) }
	return util.ProduceMapReduce(0, produce, map_, reduce, done, errCh)
}

func OperateOnJobConfigSubdirPaths(configDir, subDir string, knownInfraJobFiles sets.Set[string], callback func(*Info) error) error {
	if err := filepath.WalkDir(filepath.Join(configDir, subDir), func(path string, info fs.DirEntry, err error) error {
		logger := logrus.WithField("source-file", path)
		if err != nil {
			logger.WithError(err).Error("Failed to walk file/directory")
			return nil
		}
		if !info.IsDir() && filepath.Ext(path) == ".yaml" {
			if knownInfraJobFiles.Has(info.Name()) {
				logger.Debugf("Skipping known infra file: %s", info.Name())
				return nil
			}
			info, err := extractInfoFromPath(path)
			if err != nil {
				logger.WithError(err).Warn("Failed to determine info for prow job config")
				return nil
			}
			return callback(info)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to operator on Prow job configs: %w", err)
	}
	return nil
}

// ReadFromDir reads Prow job config from a directory and merges into one config
func ReadFromDir(dir string) (*prowconfig.JobConfig, error) {
	jobConfig := &prowconfig.JobConfig{
		PresubmitsStatic:  map[string][]prowconfig.Presubmit{},
		PostsubmitsStatic: map[string][]prowconfig.Postsubmit{},
		Periodics:         []prowconfig.Periodic{},
	}
	if err := OperateOnJobConfigDir(dir, make(sets.Set[string]), func(config *prowconfig.JobConfig, elements *Info) error {
		Append(jobConfig, config)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("failed to load all Prow jobs: %w", err)
	}

	return jobConfig, nil
}

// Append merges job configuration from part into dest
// Jobs are assumed to not overlap.
func Append(dest, part *prowconfig.JobConfig) {
	if part.PresubmitsStatic != nil {
		if dest.PresubmitsStatic == nil {
			dest.PresubmitsStatic = map[string][]prowconfig.Presubmit{}
		}
		for repo := range part.PresubmitsStatic {
			if _, ok := dest.PresubmitsStatic[repo]; ok {
				dest.PresubmitsStatic[repo] = append(dest.PresubmitsStatic[repo], part.PresubmitsStatic[repo]...)
			} else {
				dest.PresubmitsStatic[repo] = part.PresubmitsStatic[repo]
			}
		}
	}
	if part.PostsubmitsStatic != nil {
		if dest.PostsubmitsStatic == nil {
			dest.PostsubmitsStatic = map[string][]prowconfig.Postsubmit{}
		}
		for repo := range part.PostsubmitsStatic {
			if _, ok := dest.PostsubmitsStatic[repo]; ok {
				dest.PostsubmitsStatic[repo] = append(dest.PostsubmitsStatic[repo], part.PostsubmitsStatic[repo]...)
			} else {
				dest.PostsubmitsStatic[repo] = part.PostsubmitsStatic[repo]
			}
		}
	}
	dest.Periodics = append(dest.Periodics, part.Periodics...)
}

// readFromFile reads Prow job config from a YAML file
func readFromFile(path string) (*prowconfig.JobConfig, error) {
	data, err := gzip.ReadFileMaybeGZIP(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read Prow job config (%w)", err)
	}

	var jobConfig *prowconfig.JobConfig
	if err := yaml.Unmarshal(data, &jobConfig); err != nil {
		return nil, fmt.Errorf("failed to load Prow job config (%w)", err)
	}
	if jobConfig == nil { // happens when `data` is empty
		return nil, fmt.Errorf("failed to load Prow job config")
	}

	return jobConfig, nil
}

// WriteToDir takes a JobConfig and a target directory, and writes the Prow job configuration
// into files in that directory. Jobs are sharded by branch and by type. If
// target files already exist and contain Prow job configuration, the jobs will
// be merged. Jobs will be pruned based on the provided Generator that match the matchLabels set
func WriteToDir(jobDir, org, repo string, jobConfig *prowconfig.JobConfig, generator Generator, matchLabels labels.Set) error {
	allJobs := sets.Set[string]{}
	files := map[string]*prowconfig.JobConfig{}
	key := fmt.Sprintf("%s/%s", org, repo)
	for _, job := range jobConfig.PresubmitsStatic[key] {
		job.Labels[string(generator)] = string(newlyGenerated)
		job.Labels[LabelGenerator] = string(generator)
		allJobs.Insert(job.Name)
		branch := "master"
		if len(job.Branches) > 0 {
			branch = job.Branches[0]
			// branches may be regexps, strip regexp characters and trailing dashes / slashes
			branch = MakeRegexFilenameLabel(branch)
		}
		file := fmt.Sprintf("%s-%s-%s-presubmits.yaml", org, repo, branch)
		if _, ok := files[file]; ok {
			files[file].PresubmitsStatic[key] = append(files[file].PresubmitsStatic[key], job)
		} else {
			files[file] = &prowconfig.JobConfig{PresubmitsStatic: map[string][]prowconfig.Presubmit{
				key: {job},
			}}
		}
	}
	for _, job := range jobConfig.PostsubmitsStatic[key] {
		job.Labels[string(generator)] = string(newlyGenerated)
		job.Labels[LabelGenerator] = string(generator)
		allJobs.Insert(job.Name)
		branch := "master"
		if len(job.Branches) > 0 {
			branch = job.Branches[0]
			// branches may be regexps, strip regexp characters and trailing dashes / slashes
			branch = MakeRegexFilenameLabel(branch)
		}
		file := fmt.Sprintf("%s-%s-%s-postsubmits.yaml", org, repo, branch)
		if _, ok := files[file]; ok {
			files[file].PostsubmitsStatic[key] = append(files[file].PostsubmitsStatic[key], job)
		} else {
			files[file] = &prowconfig.JobConfig{PostsubmitsStatic: map[string][]prowconfig.Postsubmit{
				key: {job},
			}}
		}
	}
	for _, job := range jobConfig.Periodics {
		if len(job.ExtraRefs) == 0 {
			continue
		}
		if job.ExtraRefs[0].Org != org || job.ExtraRefs[0].Repo != repo {
			continue
		}
		job.Labels[string(generator)] = string(newlyGenerated)
		job.Labels[LabelGenerator] = string(generator)
		allJobs.Insert(job.Name)
		branch := MakeRegexFilenameLabel(job.ExtraRefs[0].BaseRef)
		file := fmt.Sprintf("%s-%s-%s-periodics.yaml", org, repo, branch)
		if _, ok := files[file]; ok {
			files[file].Periodics = append(files[file].Periodics, job)
		} else {
			files[file] = &prowconfig.JobConfig{Periodics: []prowconfig.Periodic{job}}
		}
	}

	jobDirForComponent := filepath.Join(jobDir, org, repo)
	if err := os.MkdirAll(jobDirForComponent, os.ModePerm); err != nil {
		return err
	}
	if err := OperateOnJobConfigSubdir(jobDirForComponent, "", make(sets.Set[string]), func(jobConfig *prowconfig.JobConfig, info *Info) error {
		file := filepath.Base(info.Filename)
		if generated, ok := files[file]; ok {
			delete(files, file)
			if len(generated.PresubmitsStatic) != 0 || len(generated.PostsubmitsStatic) != 0 || len(generated.Periodics) != 0 {
				mergeJobConfig(jobConfig, generated, allJobs)
				sortConfigFields(jobConfig)
			}
		}
		jobConfig, err := Prune(jobConfig, generator, matchLabels)
		if err != nil {
			return err
		}
		return WriteToFile(info.Filename, jobConfig)
	}); err != nil {
		return err
	}
	for file, jobConfig := range files {
		jobConfig, err := Prune(jobConfig, generator, matchLabels)
		if err != nil {
			return err
		}
		sortConfigFields(jobConfig)
		if err := WriteToFile(filepath.Join(jobDirForComponent, file), jobConfig); err != nil {
			return err
		}
	}
	return nil
}

// Given two JobConfig, merge jobs from the `source` one to `destination`
// one. Jobs are matched by name. All jobs from `source` will be present in
// `destination` - if there were jobs with the same name in `destination`, they
// will be updated. All jobs in `destination` that are not overwritten this
// way and are not otherwise in the set of all jobs being written stay untouched.
func mergeJobConfig(destination, source *prowconfig.JobConfig, allJobs sets.Set[string]) {
	// We do the same thing for all jobs
	if source.PresubmitsStatic != nil {
		if destination.PresubmitsStatic == nil {
			destination.PresubmitsStatic = map[string][]prowconfig.Presubmit{}
		}
		for repo, jobs := range source.PresubmitsStatic {
			oldJobs := map[string]prowconfig.Presubmit{}
			newJobs := map[string]prowconfig.Presubmit{}
			for _, job := range destination.PresubmitsStatic[repo] {
				oldJobs[job.Name] = job
			}
			for _, job := range jobs {
				newJobs[job.Name] = job
			}

			var mergedJobs []prowconfig.Presubmit
			for newJobName := range newJobs {
				newJob := newJobs[newJobName]
				if oldJob, existed := oldJobs[newJobName]; existed {
					mergedJobs = append(mergedJobs, mergePresubmits(&oldJob, &newJob))
				} else {
					mergedJobs = append(mergedJobs, newJob)
				}
			}
			for oldJobName := range oldJobs {
				if _, updated := newJobs[oldJobName]; !updated && !allJobs.Has(oldJobName) {
					mergedJobs = append(mergedJobs, oldJobs[oldJobName])
				}
			}
			destination.PresubmitsStatic[repo] = mergedJobs
		}
	}
	if source.PostsubmitsStatic != nil {
		if destination.PostsubmitsStatic == nil {
			destination.PostsubmitsStatic = map[string][]prowconfig.Postsubmit{}
		}
		for repo, jobs := range source.PostsubmitsStatic {
			oldJobs := map[string]prowconfig.Postsubmit{}
			newJobs := map[string]prowconfig.Postsubmit{}
			for _, job := range destination.PostsubmitsStatic[repo] {
				oldJobs[job.Name] = job
			}
			for _, job := range jobs {
				newJobs[job.Name] = job
			}

			var mergedJobs []prowconfig.Postsubmit
			for newJobName := range newJobs {
				newJob := newJobs[newJobName]
				if oldJob, existed := oldJobs[newJobName]; existed {
					mergedJobs = append(mergedJobs, mergePostsubmits(&oldJob, &newJob))
				} else {
					mergedJobs = append(mergedJobs, newJob)
				}
			}
			for oldJobName := range oldJobs {
				if _, updated := newJobs[oldJobName]; !updated && !allJobs.Has(oldJobName) {
					mergedJobs = append(mergedJobs, oldJobs[oldJobName])
				}
			}
			destination.PostsubmitsStatic[repo] = mergedJobs
		}
	}
	if len(source.Periodics) != 0 {
		if len(destination.Periodics) == 0 {
			destination.Periodics = []prowconfig.Periodic{}
		}
		oldJobs := map[string]prowconfig.Periodic{}
		newJobs := map[string]prowconfig.Periodic{}
		for _, job := range source.Periodics {
			newJobs[job.Name] = job
		}
		for _, job := range destination.Periodics {
			oldJobs[job.Name] = job
		}

		var mergedJobs []prowconfig.Periodic
		for newJobName := range newJobs {
			newJob := newJobs[newJobName]
			if oldJob, existed := oldJobs[newJobName]; existed {
				mergedJobs = append(mergedJobs, mergePeriodics(&oldJob, &newJob))
			} else {
				mergedJobs = append(mergedJobs, newJob)
			}
		}
		for oldJobName := range oldJobs {
			if _, updated := newJobs[oldJobName]; !updated && !allJobs.Has(oldJobName) {
				mergedJobs = append(mergedJobs, oldJobs[oldJobName])
			}
		}
		destination.Periodics = mergedJobs
	}
}

// mergePresubmits merges the two configurations, preferring fields
// in the new configuration unless the fields are set in the old
// configuration and cannot be derived from the ci-operator configuration
func mergePresubmits(old, new *prowconfig.Presubmit) prowconfig.Presubmit {
	merged := *new

	merged.MaxConcurrency = old.MaxConcurrency
	merged.SkipReport = old.SkipReport
	merged.Cluster = func() string {
		if val, exists := new.Labels[cioperatorapi.ClusterLabel]; exists {
			return val
		} else if old.Cluster != "" {
			return old.Cluster
		}
		return ""
	}()

	// TODO(muller): Special case images jobs for now. Some repos are marking
	// images jobs as optional for which we do not have syntax in ci-operator (should we?).
	// Tolerate manual changes for these jobs for now
	if strings.HasSuffix(merged.Name, "-images") {
		if old.RunIfChanged != "" || old.SkipIfOnlyChanged != "" {
			merged.RunIfChanged = old.RunIfChanged
			merged.SkipIfOnlyChanged = old.SkipIfOnlyChanged
			merged.AlwaysRun = false
		}
	}

	return merged
}

// mergePostsubmits merges the two configurations, preferring fields
// in the new configuration unless the fields are set in the old
// configuration and cannot be derived from the ci-operator configuration
func mergePostsubmits(old, new *prowconfig.Postsubmit) prowconfig.Postsubmit {
	merged := *new

	if _, ok := merged.Labels[cioperatorapi.PromotionJobLabelKey]; !ok {
		merged.MaxConcurrency = old.MaxConcurrency
	}
	if old.Cluster != "" {
		merged.Cluster = old.Cluster
	}

	return merged
}

// mergePeriodics merges the two configurations, preferring fields
// in the new configuration unless the fields are set in the old
// configuration and cannot be derived from the ci-operator configuration
func mergePeriodics(old, new *prowconfig.Periodic) prowconfig.Periodic {
	merged := *new

	merged.MaxConcurrency = old.MaxConcurrency
	//TODO(sgoeddel): We will keep this functionality for backwards-compatibility.
	// Eventually, we should only allow the reporter_config to be set through prowgen configuration
	if old.ReporterConfig != nil && merged.ReporterConfig == nil {
		merged.ReporterConfig = old.ReporterConfig
	}
	if old.Cluster != "" {
		merged.Cluster = old.Cluster
	}

	return merged
}

// sortConfigFields sorts array fields inside of job configurations so
// that their serialized form is stable and deterministic
func sortConfigFields(jobConfig *prowconfig.JobConfig) {
	for repo := range jobConfig.PresubmitsStatic {
		sort.Slice(jobConfig.PresubmitsStatic[repo], func(i, j int) bool {
			return jobConfig.PresubmitsStatic[repo][i].Name < jobConfig.PresubmitsStatic[repo][j].Name
		})
		for job := range jobConfig.PresubmitsStatic[repo] {
			if jobConfig.PresubmitsStatic[repo][job].Spec != nil {
				sortPodSpec(jobConfig.PresubmitsStatic[repo][job].Spec)
			}
		}
	}
	for repo := range jobConfig.PostsubmitsStatic {
		sort.Slice(jobConfig.PostsubmitsStatic[repo], func(i, j int) bool {
			return jobConfig.PostsubmitsStatic[repo][i].Name < jobConfig.PostsubmitsStatic[repo][j].Name
		})
		for job := range jobConfig.PostsubmitsStatic[repo] {
			if jobConfig.PostsubmitsStatic[repo][job].Spec != nil {
				sortPodSpec(jobConfig.PostsubmitsStatic[repo][job].Spec)
			}
		}
	}

	sort.Slice(jobConfig.Periodics, func(i, j int) bool {
		return jobConfig.Periodics[i].Name < jobConfig.Periodics[j].Name
	})
	for job := range jobConfig.Periodics {
		if jobConfig.Periodics[job].Spec != nil {
			sortPodSpec(jobConfig.Periodics[job].Spec)
		}
	}
}

func sortPodSpec(spec *v1.PodSpec) {
	if len(spec.Volumes) > 0 {
		sort.Slice(spec.Volumes, func(i, j int) bool {
			return spec.Volumes[i].Name < spec.Volumes[j].Name
		})
	}
	if len(spec.Containers) > 0 {
		sort.Slice(spec.Containers, func(i, j int) bool {
			return spec.Containers[i].Name < spec.Containers[j].Name
		})
		for container := range spec.Containers {
			if len(spec.Containers[container].VolumeMounts) > 0 {
				sort.Slice(spec.Containers[container].VolumeMounts, func(i, j int) bool {
					return spec.Containers[container].VolumeMounts[i].Name < spec.Containers[container].VolumeMounts[j].Name
				})
			}
			if len(spec.Containers[container].Command) == 1 && spec.Containers[container].Command[0] == "ci-operator" {
				if len(spec.Containers[container].Args) > 0 {
					sort.Strings(spec.Containers[container].Args)
				}
			}
			if len(spec.Containers[container].Env) > 0 {
				sort.Slice(spec.Containers[container].Env, func(i, j int) bool {
					return spec.Containers[container].Env[i].Name < spec.Containers[container].Env[j].Name
				})
			}
		}
	}
}

// WriteToFile writes Prow job config to a YAML file
func WriteToFile(path string, jobConfig *prowconfig.JobConfig) error {
	if len(jobConfig.PresubmitsStatic) == 0 && len(jobConfig.PostsubmitsStatic) == 0 && len(jobConfig.Periodics) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	jobConfigAsYaml, err := yaml.Marshal(*jobConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal the job config (%w)", err)
	}
	if err := os.WriteFile(path, jobConfigAsYaml, 0664); err != nil {
		return err
	}

	return nil
}

var regexParts = regexp.MustCompile(`[^\w\-.]+`)

func MakeRegexFilenameLabel(possibleRegex string) string {
	label := regexParts.ReplaceAllString(possibleRegex, "")
	label = strings.TrimLeft(strings.TrimRight(label, "-._"), "-._")
	if len(label) == 0 {
		label = "master"
	}
	return label
}

// IsGenerated returns true if the job was generated by the provided Generator
func IsGenerated(job prowconfig.JobBase, generator Generator) (bool, error) {
	generatedSelector, err := generatedSelectorFor(generator)
	if err != nil {
		return false, err
	}
	return generatedSelector.Matches(labels.Set(job.Labels)), nil
}

func generatedSelectorFor(generator Generator) (labels.Selector, error) {
	requirement, err := labels.NewRequirement(LabelGenerator, selection.Equals, []string{string(generator)})
	if err != nil {
		return nil, err
	}
	return labels.NewSelector().Add(*requirement), nil
}

func staleSelectorFor(generator Generator, pruneLabels labels.Set) (labels.Selector, error) {
	ls, err := generatedSelectorFor(generator)
	if err != nil {
		return nil, err
	}
	notNewlyGenerated, err := labels.NewRequirement(string(generator), selection.NotEquals, []string{string(newlyGenerated)})
	if err != nil {
		return nil, err
	}
	ls = ls.Add(*notNewlyGenerated)
	for label, value := range pruneLabels {
		req, err := labels.NewRequirement(label, selection.Equals, []string{value})
		if err != nil {
			return nil, err
		}
		ls = ls.Add(*req)
	}
	return ls, nil
}

// Prune removes all generated jobs of the supplied Generator with values that are NOT newly-generated.
// Prune() returns the resulting job config (which may even be completely empty).
func Prune(jobConfig *prowconfig.JobConfig, generator Generator, pruneLabels labels.Set) (*prowconfig.JobConfig, error) {
	var pruned prowconfig.JobConfig
	staleSelector, err := staleSelectorFor(generator, pruneLabels)
	if err != nil {
		return nil, err
	}
	isStale := func(job prowconfig.JobBase) bool {
		return staleSelector.Matches(labels.Set(job.Labels))
	}
	generatedSelector, err := generatedSelectorFor(generator)
	if err != nil {
		return nil, err
	}
	isGenerated := func(job prowconfig.JobBase) bool {
		return generatedSelector.Matches(labels.Set(job.Labels))
	}

	for repo, jobs := range jobConfig.PresubmitsStatic {
		for _, job := range jobs {
			if isStale(job.JobBase) {
				continue
			}
			// The job base might be shared with other job objects.
			// We make a copy here to avoid the intervention in some corner cases identified for DPTP-3845.
			// A better solution is to not share in the input but fix on the caller here is simpler.
			var copy prowconfig.Presubmit
			if err := deepcopy.Copy(&copy, &job); err != nil {
				return nil, fmt.Errorf("failed to deepcopy: %w", err)
			}
			if isGenerated(copy.JobBase) {
				delete(copy.Labels, string(generator))
			}

			if pruned.PresubmitsStatic == nil {
				pruned.PresubmitsStatic = map[string][]prowconfig.Presubmit{}
			}

			pruned.PresubmitsStatic[repo] = append(pruned.PresubmitsStatic[repo], copy)
		}
	}

	for repo, jobs := range jobConfig.PostsubmitsStatic {
		for _, job := range jobs {
			if isStale(job.JobBase) {
				continue
			}
			if isGenerated(job.JobBase) {
				delete(job.Labels, string(generator))
			}
			if pruned.PostsubmitsStatic == nil {
				pruned.PostsubmitsStatic = map[string][]prowconfig.Postsubmit{}
			}

			pruned.PostsubmitsStatic[repo] = append(pruned.PostsubmitsStatic[repo], job)
		}
	}

	for _, job := range jobConfig.Periodics {
		if isStale(job.JobBase) {
			continue
		}
		var copy prowconfig.Periodic
		if err := deepcopy.Copy(&copy, &job); err != nil {
			return nil, fmt.Errorf("failed to deepcopy: %w", err)
		}
		if isGenerated(copy.JobBase) {
			delete(copy.Labels, string(generator))
		}

		pruned.Periodics = append(pruned.Periodics, copy)
	}

	return &pruned, nil
}

// FeatureBranch returns a regex string that matches feature branch prefixes for the given branch name:
// I.e. returns '^master-' for 'master'. If the given branch name already looks like a regex,
// return it unchanged.
func FeatureBranch(branch string) string {
	if !SimpleBranchRegexp.MatchString(branch) {
		return branch
	}
	return fmt.Sprintf("^%s-", regexp.QuoteMeta(branch))
}

// ExactlyBranch returns a regex string that matches exactly the given branch name: I.e. returns
// '^master$' for 'master'. If the given branch name already looks like a regex, return it unchanged.
func ExactlyBranch(branch string) string {
	if !SimpleBranchRegexp.MatchString(branch) {
		return branch
	}
	return fmt.Sprintf("^%s$", regexp.QuoteMeta(branch))
}
