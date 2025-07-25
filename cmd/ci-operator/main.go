package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bombsimon/logrusr/v3"
	"github.com/go-logr/logr"
	egressfirewallv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	"github.com/sirupsen/logrus"

	appsv1 "k8s.io/api/apps/v1"
	authapi "k8s.io/api/authorization/v1"
	coreapi "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacapi "k8s.io/api/rbac/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/scheme"
	authclientset "k8s.io/client-go/kubernetes/typed/authorization/v1"
	coreclientset "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilpointer "k8s.io/utils/pointer"
	controllerruntime "sigs.k8s.io/controller-runtime"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	crcontrollerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrlruntimelog "sigs.k8s.io/controller-runtime/pkg/log"
	prowapi "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	_ "sigs.k8s.io/prow/pkg/cache"
	"sigs.k8s.io/prow/pkg/config/secret"
	prowio "sigs.k8s.io/prow/pkg/io"
	"sigs.k8s.io/prow/pkg/logrusutil"
	"sigs.k8s.io/prow/pkg/pod-utils/downwardapi"
	"sigs.k8s.io/prow/pkg/version"
	csiapi "sigs.k8s.io/secrets-store-csi-driver/apis/v1"
	"sigs.k8s.io/yaml"

	buildv1 "github.com/openshift/api/build/v1"
	imageapi "github.com/openshift/api/image/v1"
	projectapi "github.com/openshift/api/project/v1"
	routev1 "github.com/openshift/api/route/v1"
	templateapi "github.com/openshift/api/template/v1"
	buildclientset "github.com/openshift/client-go/build/clientset/versioned/typed/build/v1"
	projectclientset "github.com/openshift/client-go/project/clientset/versioned"
	templatescheme "github.com/openshift/client-go/template/clientset/versioned/scheme"
	templateclientset "github.com/openshift/client-go/template/clientset/versioned/typed/template/v1"
	hivev1 "github.com/openshift/hive/apis/hive/v1"

	"github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/api/configresolver"
	"github.com/openshift/ci-tools/pkg/api/nsttl"
	"github.com/openshift/ci-tools/pkg/defaults"
	"github.com/openshift/ci-tools/pkg/interrupt"
	"github.com/openshift/ci-tools/pkg/junit"
	"github.com/openshift/ci-tools/pkg/labeledclient"
	"github.com/openshift/ci-tools/pkg/lease"
	"github.com/openshift/ci-tools/pkg/load"
	"github.com/openshift/ci-tools/pkg/metrics"
	"github.com/openshift/ci-tools/pkg/registry"
	"github.com/openshift/ci-tools/pkg/registry/server"
	"github.com/openshift/ci-tools/pkg/results"
	"github.com/openshift/ci-tools/pkg/secrets"
	"github.com/openshift/ci-tools/pkg/steps"
	"github.com/openshift/ci-tools/pkg/util"
	"github.com/openshift/ci-tools/pkg/util/gzip"
	"github.com/openshift/ci-tools/pkg/validation"
)

const usage = `Orchestrate multi-stage image-based builds

The ci-operator reads a declarative configuration YAML file and executes a set of build
steps on an OpenShift cluster for image-based components. By default, all steps are run,
but a caller may select one or more targets (image names or test names) to limit to only
steps that those targets depend on. The build creates a new project to run the builds in
and can automatically clean up the project when the build completes.

ci-operator leverages declarative OpenShift builds and images to reuse previously compiled
artifacts. It makes building multiple images that share one or more common base layers
simple as well as running tests that depend on those images.

Since the command is intended for use in CI environments it requires an input environment
variable called the JOB_SPEC that defines the GitHub project to execute and the commit,
branch, and any PRs to merge onto the branch. See the kubernetes/test-infra project for
a description of JOB_SPEC.

The inputs of the build (source code, tagged images, configuration) are combined to form
a consistent name for the target namespace that will change if any of the inputs change.
This allows multiple test jobs to share common artifacts and still perform retries.

The standard build steps are designed for simple command-line actions (like invoking
"make test") but can be extended by passing one or more templates via the --template flag.
The name of the template defines the stage and the template must contain at least one
pod. The parameters passed to the template are the current process environment and a set
of dynamic parameters that are inferred from previous steps. These parameters are:

  NAMESPACE
    The namespace generated by the operator for the given inputs or the value of
    --namespace.

  IMAGE_FORMAT
    A string that points to the public image repository URL of the image stream(s)
    created by the tag step. Example:

      registry.svc.ci.openshift.org/ci-op-9o8bacu/stable:${component}

    Will cause the template to depend on all image builds.

  IMAGE_<component>
    The public image repository URL for an output image. If specified the template
    will depend on the image being built.

  LOCAL_IMAGE_<component>
    The public image repository URL for an image that was built during this run but
    was not part of the output (such as pipeline cache images). If specified the
    template will depend on the image being built.

  JOB_NAME
    The job name from the JOB_SPEC

  JOB_NAME_SAFE
    The job name in a form safe for use as a Kubernetes resource name.

  JOB_NAME_HASH
    A short hash of the job name for making tasks unique. This will not account for the target-additional-suffix.

  UNIQUE_HASH
	A hash for making tasks unique, even when the job name may be the same due to using the target-additional-suffix.

  RPM_REPO_<org>_<repo>
    If the job creates RPMs this will be the public URL that can be used as the
		baseurl= value of an RPM repository. The value of org and repo are uppercased
		and dashes are replaced with underscores.

Dynamic environment variables are overridden by process environment variables.

Both test and template jobs can gather artifacts created by pods. Set
--artifact-dir to define the top level artifact directory, and any test task
that defines artifact_dir or template that has an "artifacts" volume mounted
into a container will have artifacts extracted after the container has completed.
Errors in artifact extraction will not cause build failures.

In CI environments the inputs to a job may be different than what a normal
development workflow would use. The --override file will override fields
defined in the config file, such as base images and the release tag configuration.

After a successful build the --promote will tag each built image (in "images")
to the image stream(s) identified by the "promotion" config. You may add
additional images to promote and their target names via the "additional_images"
map.
`

const (
	leaseAcquireTimeout = 120 * time.Minute
)

var (
	// leaseServerAddress is the default lease server in app.ci
	leaseServerAddress = api.URLForService(api.ServiceBoskos)
	// configResolverAddress is the default configresolver address in app.ci
	configResolverAddress = api.URLForService(api.ServiceConfig)
)

// CustomProwMetadata the name of the custom prow metadata file that's expected to be found in the artifacts directory.
const CustomProwMetadata = "custom-prow-metadata.json"

func main() {
	censor, closer, err := setupLogger()
	if err != nil {
		logrus.WithError(err).Fatal("Could not set up logging.")
	}
	if closer != nil {
		defer func() {
			if err := closer.Close(); err != nil {
				logrus.WithError(err).Warn("Could not close ci-operator log file.")
			}
		}()
	}
	// "i just don't want spam"
	klog.LogToStderr(false)
	logrus.Infof("%s version %s", version.Name, version.Version)
	flagSet := flag.NewFlagSet("", flag.ExitOnError)
	opt := bindOptions(flagSet)
	opt.censor = censor
	if err := flagSet.Parse(os.Args[1:]); err != nil {
		logrus.WithError(err).Fatal("failed to parse flags")
	}

	ctrlruntimelog.SetLogger(logr.New(ctrlruntimelog.NullLogSink{}))
	if opt.verbose {
		fs := flag.NewFlagSet("", flag.ExitOnError)
		klog.InitFlags(fs)
		if err := fs.Set("alsologtostderr", "true"); err != nil {
			logrus.WithError(err).Fatal("could not set klog alsologtostderr")
		}
		if err := fs.Set("v", "10"); err != nil {
			logrus.WithError(err).Fatal("could not set klog v")
		}
		if err := fs.Parse([]string{}); err != nil {
			logrus.WithError(err).Fatal("failed to parse klog flags")
		}

		logrus.SetLevel(logrus.TraceLevel)
		logrus.SetFormatter(&logrus.JSONFormatter{})
		logrus.SetReportCaller(true)
		controllerruntime.SetLogger(logrusr.New(logrus.StandardLogger()))
	}
	if opt.help {
		fmt.Print(usage)
		flagSet.SetOutput(os.Stdout)
		flagSet.Usage()
		os.Exit(0)
	}
	flagSet.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "delete-when-idle":
			opt.idleCleanupDurationSet = true
		case "delete-after":
			opt.cleanupDurationSet = true
		}
	})
	if err := addSchemes(); err != nil {
		logrus.WithError(err).Fatal("failed to set up scheme")
	}

	rand.Seed(time.Now().UnixNano())

	if err := opt.Complete(); err != nil {
		logrus.WithError(err).Error("Failed to load arguments.")
		opt.Report(results.ForReason("loading_args").ForError(err))
		os.Exit(1)
	}

	ctx := context.TODO()
	metricsClient, err := ctrlruntimeclient.New(opt.clusterConfig, ctrlruntimeclient.Options{})
	if err != nil {
		logrus.WithError(err).Fatal("Failed to create the metrics client")
	}
	opt.metricsAgent = metrics.NewMetricsAgent(ctx, metricsClient)
	go opt.metricsAgent.Run()

	opt.metricsAgent.Record(
		&metrics.InsightsEvent{
			Name:              "ci_operator_started",
			AdditionalContext: map[string]any{"job_spec": opt.jobSpec},
		},
	)

	if errs := opt.Run(); len(errs) > 0 {
		var defaulted []error
		for _, err := range errs {
			defaulted = append(defaulted, results.DefaultReason(err))
		}

		message := bytes.Buffer{}
		for _, err := range errs {
			message.WriteString(fmt.Sprintf("\n  * %s", err.Error()))
		}
		logrus.Error("Some steps failed:")
		logrus.Error(message.String())

		opt.metricsAgent.Stop()
		opt.Report(defaulted...)

		os.Exit(1)
	}
	opt.Report()
	opt.metricsAgent.Stop()
}

// setupLogger sets up logrus to print all logs to a file and user-friendly logs to stdout
func setupLogger() (*secrets.DynamicCensor, io.Closer, error) {
	logrus.SetLevel(logrus.TraceLevel)
	censor := secrets.NewDynamicCensor()
	logrus.SetFormatter(logrusutil.NewFormatterWithCensor(logrus.StandardLogger().Formatter, &censor))
	logrus.SetOutput(io.Discard)
	logrus.AddHook(&formattingHook{
		formatter: logrusutil.NewFormatterWithCensor(&logrus.TextFormatter{
			ForceColors:     true,
			DisableQuote:    true,
			FullTimestamp:   true,
			TimestampFormat: time.RFC3339,
		}, &censor),
		writer: os.Stdout,
		logLevels: []logrus.Level{
			logrus.InfoLevel,
			logrus.WarnLevel,
			logrus.ErrorLevel,
			logrus.FatalLevel,
			logrus.PanicLevel,
		},
	})
	artifactDir, set := api.Artifacts()
	if !set {
		return &censor, nil, nil
	}
	if err := os.MkdirAll(artifactDir, 0777); err != nil {
		return nil, nil, err
	}
	verboseFile, err := os.Create(filepath.Join(artifactDir, "ci-operator.log"))
	if err != nil {
		return nil, nil, err
	}
	logrus.AddHook(&formattingHook{
		formatter: logrusutil.NewFormatterWithCensor(&logrus.JSONFormatter{}, &censor),
		writer:    verboseFile,
		logLevels: logrus.AllLevels,
	})
	return &censor, verboseFile, nil
}

type formattingHook struct {
	formatter logrus.Formatter
	writer    io.Writer
	logLevels []logrus.Level
}

func (hook *formattingHook) Fire(entry *logrus.Entry) error {
	line, err := hook.formatter.Format(entry)
	if err != nil {
		return err
	}
	_, err = hook.writer.Write(line)
	return err
}

func (hook *formattingHook) Levels() []logrus.Level {
	return hook.logLevels
}

type stringSlice struct {
	values []string
}

func (s *stringSlice) String() string {
	return strings.Join(s.values, string(filepath.Separator))
}

func (s *stringSlice) Set(value string) error {
	s.values = append(s.values, value)
	return nil
}

type options struct {
	configSpecPath       string
	unresolvedConfigPath string
	templatePaths        stringSlice
	secretDirectories    stringSlice
	sshKeyPath           string
	oauthTokenPath       string

	targets stringSlice
	promote bool

	verbose    bool
	help       bool
	printGraph bool

	writeParams string
	artifactDir string

	gitRef                 string
	namespace              string
	baseNamespace          string
	extraInputHash         stringSlice
	idleCleanupDuration    time.Duration
	idleCleanupDurationSet bool
	cleanupDuration        time.Duration
	cleanupDurationSet     bool

	inputHash                  string
	secrets                    []*coreapi.Secret
	templates                  []*templateapi.Template
	graphConfig                api.GraphConfiguration
	configSpec                 *api.ReleaseBuildConfiguration
	jobSpec                    *api.JobSpec
	clusterConfig              *rest.Config
	podPendingTimeout          time.Duration
	consoleHost                string
	nodeName                   string
	leaseServer                string
	leaseServerCredentialsFile string
	leaseAcquireTimeout        time.Duration
	leaseClient                lease.Client
	clusterProfiles            []clusterProfileForTarget

	givePrAuthorAccessToNamespace bool
	impersonateUser               string
	authors                       []string

	resolverAddress string
	resolverClient  server.ResolverClient

	registryPath string
	org          string
	repo         string
	branch       string
	variant      string

	injectTest string

	metadataRevision int

	pullSecretPath string
	pullSecret     *coreapi.Secret

	pushSecretPath string
	pushSecret     *coreapi.Secret

	uploadSecretPath string
	uploadSecret     *coreapi.Secret

	cloneAuthConfig *steps.CloneAuthConfig

	resultsOptions results.Options

	censor *secrets.DynamicCensor

	hiveKubeconfigPath string
	hiveKubeconfig     *rest.Config

	multiStageParamOverrides stringSlice
	dependencyOverrides      stringSlice

	targetAdditionalSuffix string
	manifestToolDockerCfg  string
	localRegistryDNS       string

	restrictNetworkAccess       bool
	enableSecretsStoreCSIDriver bool

	metricsAgent *metrics.MetricsAgent
}

func bindOptions(flag *flag.FlagSet) *options {
	opt := &options{
		idleCleanupDuration: 1 * time.Hour,
		cleanupDuration:     24 * time.Hour,
	}

	// command specific options
	flag.BoolVar(&opt.help, "h", false, "short for --help")
	flag.BoolVar(&opt.help, "help", false, "See help for this command.")
	flag.BoolVar(&opt.verbose, "v", false, "Show verbose output.")

	// what we will run
	flag.StringVar(&opt.nodeName, "node", "", "Restrict scheduling of pods to a single node in the cluster. Does not afffect indirectly created pods (e.g. builds).")
	flag.DurationVar(&opt.podPendingTimeout, "pod-pending-timeout", 60*time.Minute, "Maximum amount of time created pods can spend before the running state. For test pods, this applies to each container. For builds, it applies to the build execution as a whole.")
	flag.StringVar(&opt.leaseServer, "lease-server", leaseServerAddress, "Address of the server that manages leases. Required if any test is configured to acquire a lease.")
	flag.StringVar(&opt.leaseServerCredentialsFile, "lease-server-credentials-file", "", "The path to credentials file used to access the lease server. The content is of the form <username>:<password>.")
	flag.DurationVar(&opt.leaseAcquireTimeout, "lease-acquire-timeout", leaseAcquireTimeout, "Maximum amount of time to wait for lease acquisition")
	flag.StringVar(&opt.registryPath, "registry", "", "Path to the step registry directory")
	flag.StringVar(&opt.configSpecPath, "config", "", "The configuration file. If not specified the CONFIG_SPEC environment variable or the configresolver will be used.")
	flag.StringVar(&opt.unresolvedConfigPath, "unresolved-config", "", "The configuration file, before resolution. If not specified the UNRESOLVED_CONFIG environment variable will be used, if set.")
	flag.Var(&opt.targets, "target", "One or more targets in the configuration to build. Only steps that are required for this target will be run.")
	flag.BoolVar(&opt.printGraph, "print-graph", opt.printGraph, "Print a directed graph of the build steps and exit. Intended for use with the golang digraph utility.")

	// add to the graph of things we run or create
	flag.Var(&opt.templatePaths, "template", "A set of paths to optional templates to add as stages to this job. Each template is expected to contain at least one restart=Never pod. Parameters are filled from environment or from the automatic parameters generated by the operator.")
	flag.Var(&opt.secretDirectories, "secret-dir", "One or more directories that should converted into secrets in the test namespace. If the directory contains a single file with name .dockercfg or config.json it becomes a pull secret.")
	flag.StringVar(&opt.sshKeyPath, "ssh-key-path", "", "A path of the private ssh key that is going to be used to clone a private repository.")
	flag.StringVar(&opt.oauthTokenPath, "oauth-token-path", "", "A path of the OAuth token that is going to be used to clone a private repository.")

	// the target namespace and cleanup behavior
	flag.Var(&opt.extraInputHash, "input-hash", "Add arbitrary inputs to the build input hash to make the created namespace unique.")
	flag.StringVar(&opt.namespace, "namespace", "", "Namespace to create builds into, defaults to build_id from JOB_SPEC. If the string '{id}' is in this value it will be replaced with the build input hash.")
	flag.StringVar(&opt.baseNamespace, "base-namespace", "stable", "Namespace to read builds from, defaults to stable.")
	flag.DurationVar(&opt.idleCleanupDuration, "delete-when-idle", opt.idleCleanupDuration, "If no pod is running for longer than this interval, delete the namespace. Set to zero to retain the contents. Requires the namespace TTL controller to be deployed.")
	flag.DurationVar(&opt.cleanupDuration, "delete-after", opt.cleanupDuration, "If namespace exists for longer than this interval, delete the namespace. Set to zero to retain the contents. Requires the namespace TTL controller to be deployed.")

	// actions to add to the graph
	flag.BoolVar(&opt.promote, "promote", false, "When all other targets complete, publish the set of images built by this job into the release configuration.")

	// output control
	flag.StringVar(&opt.artifactDir, "artifact-dir", "", "DEPRECATED. Does nothing, set $ARTIFACTS instead.")
	flag.StringVar(&opt.writeParams, "write-params", "", "If set write an env-compatible file with the output of the job.")

	// experimental flags
	flag.StringVar(&opt.gitRef, "git-ref", "", "Populate the job spec from this local Git reference. If JOB_SPEC is set, the refs field will be overwritten.")
	flag.BoolVar(&opt.givePrAuthorAccessToNamespace, "give-pr-author-access-to-namespace", true, "Give view access to the temporarily created namespace to the PR author.")
	flag.StringVar(&opt.impersonateUser, "as", "", "Username to impersonate")
	flag.BoolVar(&opt.restrictNetworkAccess, "restrict-network-access", false, "Restrict network access to 10.0.0.0/8 (RedHat intranet).")
	flag.BoolVar(&opt.enableSecretsStoreCSIDriver, "enable-secrets-store-csi-driver", false, "Use Secrets Store CSI driver for accessing multi-stage credentials.")

	// flags needed for the configresolver
	flag.StringVar(&opt.resolverAddress, "resolver-address", configResolverAddress, "Address of configresolver")
	flag.StringVar(&opt.org, "org", "", "Org of the project (used by configresolver)")
	flag.StringVar(&opt.repo, "repo", "", "Repo of the project (used by configresolver)")
	flag.StringVar(&opt.branch, "branch", "", "Branch of the project (used by configresolver)")
	flag.StringVar(&opt.variant, "variant", "", "Variant of the project's ci-operator config (used by configresolver)")

	flag.StringVar(&opt.injectTest, "with-test-from", "", "Inject a test from another ci-operator config, specified by ORG/REPO@BRANCH{__VARIANT}:TEST or JSON (used by configresolver)")

	flag.StringVar(&opt.pullSecretPath, "image-import-pull-secret", "", "A set of dockercfg credentials used to import images for the tag_specification.")
	flag.StringVar(&opt.pushSecretPath, "image-mirror-push-secret", "", "A set of dockercfg credentials used to mirror images for the promotion.")
	flag.StringVar(&opt.uploadSecretPath, "gcs-upload-secret", "", "GCS credentials used to upload logs and artifacts.")

	flag.StringVar(&opt.hiveKubeconfigPath, "hive-kubeconfig", "", "Path to the kubeconfig file to use for requests to Hive.")

	flag.Var(&opt.multiStageParamOverrides, "multi-stage-param", "A repeatable option where one or more environment parameters can be passed down to the multi-stage steps. This parameter should be in the format NAME=VAL. e.g --multi-stage-param PARAM1=VAL1 --multi-stage-param PARAM2=VAL2.")
	flag.Var(&opt.dependencyOverrides, "dependency-override-param", "A repeatable option used to override dependencies with external pull specs. This parameter should be in the format ENVVARNAME=PULLSPEC, e.g. --dependency-override-param=OO_INDEX=registry.mydomain.com:5000/pushed/myimage. This would override the value for the OO_INDEX environment variable for any tests/steps that currently have that dependency configured.")

	flag.StringVar(&opt.targetAdditionalSuffix, "target-additional-suffix", "", "Inject an additional suffix onto the targeted test's 'as' name. Used for adding an aggregate index")

	flag.StringVar(&opt.manifestToolDockerCfg, "manifest-tool-dockercfg", "/secrets/manifest-tool/.dockerconfigjson", "The dockercfg file path to be used to push the manifest listed image after build. This is being used by the manifest-tool binary.")
	flag.StringVar(&opt.localRegistryDNS, "local-registry-dns", "image-registry.openshift-image-registry.svc:5000", "Defines the target image registry.")

	opt.resultsOptions.Bind(flag)
	return opt
}

func (o *options) Complete() error {
	jobSpec, err := api.ResolveSpecFromEnv()
	if err != nil {
		if len(o.gitRef) == 0 {
			return fmt.Errorf("failed to determine job spec: no --git-ref passed and failed to resolve job spec from env: %w", err)
		}
		// Failed to read $JOB_SPEC but --git-ref was passed, so try that instead
		spec, refErr := jobSpecFromGitRef(o.gitRef)
		if refErr != nil {
			return fmt.Errorf("failed to determine job spec: failed to resolve --git-ref: %w", refErr)
		}
		jobSpec = spec
	} else if len(o.gitRef) > 0 {
		// Read from $JOB_SPEC but --git-ref was also passed, so merge them
		spec, err := jobSpecFromGitRef(o.gitRef)
		if err != nil {
			return fmt.Errorf("failed to determine job spec: failed to resolve --git-ref: %w", err)
		}
		jobSpec.Refs = spec.Refs
	}
	jobSpec.BaseNamespace = o.baseNamespace
	target := "all"
	if len(o.targets.values) > 0 {
		target = o.targets.values[0]
	}
	o.jobSpec = jobSpec
	o.jobSpec.Target = target

	info := o.getResolverInfo(jobSpec)
	o.resolverClient = server.NewResolverClient(o.resolverAddress)

	if o.unresolvedConfigPath != "" && o.configSpecPath != "" {
		return errors.New("cannot set --config and --unresolved-config at the same time")
	}
	if o.unresolvedConfigPath != "" && o.resolverAddress == "" {
		return errors.New("cannot request resolved config with --unresolved-config unless providing --resolver-address")
	}

	injectTest, err := o.getInjectTest()
	if err != nil {
		return err
	}

	var config *api.ReleaseBuildConfiguration
	if injectTest != nil {
		if o.resolverAddress == "" {
			return errors.New("cannot request config with injected test without providing --resolver-address")
		}
		if o.unresolvedConfigPath != "" || o.configSpecPath != "" {
			return errors.New("cannot request injecting test into locally provided config")
		}
		config, err = o.resolverClient.ConfigWithTest(info, injectTest)
	} else {
		var opener prowio.Opener
		if _, set := os.LookupEnv(configSpecGcsUrlVar); set { // The opener is only needed when we may have to read from a GCS bucket
			opener, err = prowio.NewOpener(context.Background(), o.uploadSecretPath, "")
			if err != nil {
				logrus.WithError(err).Fatalf("Error creating opener to read %s", configSpecGcsUrlVar)
			}
		}
		config, err = o.loadConfig(info, bucketReader{opener: opener})
	}

	if err != nil {
		return results.ForReason("loading_config").WithError(err).Errorf("failed to load configuration: %v", err)
	}

	if len(o.gitRef) != 0 && config.CanonicalGoRepository != nil {
		o.jobSpec.Refs.PathAlias = *config.CanonicalGoRepository
	}
	o.configSpec = config
	o.jobSpec.Metadata = config.Metadata
	mergedConfig := o.injectTest != ""
	if err := validation.IsValidResolvedConfiguration(o.configSpec, mergedConfig); err != nil {
		return results.ForReason("validating_config").ForError(err)
	}
	o.graphConfig = defaults.FromConfigStatic(o.configSpec)
	if err := validation.IsValidGraphConfiguration(o.graphConfig.Steps); err != nil {
		return results.ForReason("validating_config").ForError(err)
	}

	if o.verbose {
		config, _ := yaml.Marshal(o.configSpec)
		logrus.WithField("config", string(config)).Trace("Resolved configuration.")
		job, _ := json.Marshal(o.jobSpec)
		logrus.WithField("jobspec", string(job)).Trace("Resolved job spec.")
	}

	var refs []prowapi.Refs
	if o.jobSpec.Refs != nil {
		refs = append(refs, *o.jobSpec.Refs)
	}
	refs = append(refs, o.jobSpec.ExtraRefs...)

	if len(refs) == 0 {
		logrus.Info("No source defined")
	}
	for _, ref := range refs {
		if ref.BaseSHA == "" {
			logrus.Debugf("Resolved SHA missing for %s in https://github.com/%s/%s: adding synthetic input to avoid false cache hit", ref.BaseRef, ref.Org, ref.Repo)
			o.extraInputHash.values = append(o.extraInputHash.values, time.Now().String())
		}
		logrus.Info(summarizeRef(ref))

		for _, pull := range ref.Pulls {
			o.authors = append(o.authors, pull.Author)
		}
	}

	if len(o.sshKeyPath) > 0 && len(o.oauthTokenPath) > 0 {
		return errors.New("both --ssh-key-path and --oauth-token-path are specified")
	}

	var cloneAuthSecretPath string
	if len(o.oauthTokenPath) > 0 {
		cloneAuthSecretPath = o.oauthTokenPath
		o.cloneAuthConfig = &steps.CloneAuthConfig{Type: steps.CloneAuthTypeOAuth}
	} else if len(o.sshKeyPath) > 0 {
		cloneAuthSecretPath = o.sshKeyPath
		o.cloneAuthConfig = &steps.CloneAuthConfig{Type: steps.CloneAuthTypeSSH}
	}

	if len(cloneAuthSecretPath) > 0 {
		o.cloneAuthConfig.Secret, err = getCloneSecretFromPath(o.cloneAuthConfig.Type, cloneAuthSecretPath)
		if err != nil {
			return fmt.Errorf("could not get secret from path %s: %w", cloneAuthSecretPath, err)
		}
	}

	for _, path := range o.secretDirectories.values {
		secret, err := util.SecretFromDir(path)
		name := filepath.Base(path)
		if err != nil {
			return fmt.Errorf("failed to generate secret %s: %w", name, err)
		}
		secret.Name = name
		if len(secret.Data) == 1 {
			if _, ok := secret.Data[coreapi.DockerConfigJsonKey]; ok {
				secret.Type = coreapi.SecretTypeDockerConfigJson
			}
			if _, ok := secret.Data[coreapi.DockerConfigKey]; ok {
				secret.Type = coreapi.SecretTypeDockercfg
			}
		}
		o.secrets = append(o.secrets, secret)
	}

	o.getClusterProfileNamesFromTargets()

	for _, path := range o.templatePaths.values {
		contents, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("could not read dir %s for template: %w", path, err)
		}
		obj, gvk, err := templatescheme.Codecs.UniversalDeserializer().Decode(contents, nil, nil)
		if err != nil {
			return fmt.Errorf("unable to parse template %s: %w", path, err)
		}
		template, ok := obj.(*templateapi.Template)
		if !ok {
			return fmt.Errorf("%s is not a template: %v", path, gvk)
		}
		if len(template.Name) == 0 {
			template.Name = filepath.Base(path)
			template.Name = strings.TrimSuffix(template.Name, filepath.Ext(template.Name))
		}
		o.templates = append(o.templates, template)
	}

	clusterConfig, err := util.LoadClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to load cluster config: %w", err)
	}

	if len(o.impersonateUser) > 0 {
		clusterConfig.Impersonate = rest.ImpersonationConfig{UserName: o.impersonateUser}
	}

	if o.verbose {
		clusterConfig.ContentType = "application/json"
		clusterConfig.AcceptContentTypes = "application/json"
	}

	o.clusterConfig = clusterConfig

	if o.pullSecretPath != "" {
		if o.pullSecret, err = getDockerConfigSecret(api.RegistryPullCredentialsSecret, o.pullSecretPath); err != nil {
			return fmt.Errorf("could not get pull secret %s from path %s: %w", api.RegistryPullCredentialsSecret, o.pullSecretPath, err)
		}
	}
	if o.pushSecretPath != "" {
		if o.pushSecret, err = getDockerConfigSecret(api.RegistryPushCredentialsCICentralSecret, o.pushSecretPath); err != nil {
			return fmt.Errorf("could not get push secret %s from path %s: %w", api.RegistryPushCredentialsCICentralSecret, o.pushSecretPath, err)
		}
	}

	if o.uploadSecretPath != "" {
		gcsSecretName := resolveGCSCredentialsSecret(o.jobSpec)
		if o.uploadSecret, err = getSecret(gcsSecretName, o.uploadSecretPath); err != nil {
			return fmt.Errorf("could not get upload secret %s from path %s: %w", gcsSecretName, o.uploadSecretPath, err)
		}
	}

	if o.hiveKubeconfigPath != "" {
		kubeConfig, err := util.LoadKubeConfig(o.hiveKubeconfigPath)
		if err != nil {
			return fmt.Errorf("could not load Hive kube config from path %s: %w", o.hiveKubeconfigPath, err)
		}
		o.hiveKubeconfig = kubeConfig
	}

	applyEnvOverrides(o)

	if err := overrideMultiStageParams(o); err != nil {
		return err
	}

	handleTargetAdditionalSuffix(o)

	return overrideTestStepDependencyParams(o)
}

func parseKeyValParams(input []string, paramType string) (map[string]string, error) {
	var validationErrors []error
	params := make(map[string]string)
	for _, param := range input {
		paramNameAndVal := strings.Split(param, "=")
		if len(paramNameAndVal) == 2 {
			params[strings.TrimSpace(paramNameAndVal[0])] = strings.TrimSpace(paramNameAndVal[1])
		} else {
			validationErrors = append(validationErrors, fmt.Errorf("could not parse %s: %s is not in the format key=value", paramType, param))
		}
	}

	if len(validationErrors) > 0 {
		return nil, utilerrors.NewAggregate(validationErrors)
	}

	return params, nil
}

func handleTargetAdditionalSuffix(o *options) {
	if o.targetAdditionalSuffix == "" {
		return
	}
	o.jobSpec.TargetAdditionalSuffix = o.targetAdditionalSuffix
	for i, test := range o.configSpec.Tests {
		for j, target := range o.targets.values {
			if test.As == target {
				targetWithSuffix := fmt.Sprintf("%s-%s", test.As, o.targetAdditionalSuffix)
				o.configSpec.Tests[i].As = targetWithSuffix
				if j == 0 { //only set if it is the first target
					o.jobSpec.Target = targetWithSuffix
				}
				o.targets.values[j] = targetWithSuffix
				logrus.Debugf("added suffix to target, now: %s", test.As)
				break
			}
		}
	}
}

func overrideMultiStageParams(o *options) error {
	// see if there are any passed-in multi-stage parameters.
	if len(o.multiStageParamOverrides.values) == 0 {
		return nil
	}

	multiStageParams, err := parseKeyValParams(o.multiStageParamOverrides.values, "multi-stage-param")

	if err != nil {
		return err
	}

	// for any multi-stage tests, go ahead and inject the passed-in parameters. Note that parameters explicitly passed
	// in to ci-operator will take precedence.
	for _, test := range o.configSpec.Tests {
		if test.MultiStageTestConfigurationLiteral != nil {
			if test.MultiStageTestConfigurationLiteral.Environment == nil {
				test.MultiStageTestConfigurationLiteral.Environment = make(api.TestEnvironment)
			}

			for paramName, paramVal := range multiStageParams {
				valueWithoutQuotes := strings.Trim(paramVal, `"'`)
				test.MultiStageTestConfigurationLiteral.Environment[paramName] = valueWithoutQuotes
			}
		}
	}

	return nil
}

// applyEnvOverrides processes environment variables with override prefixes and applies them to the test configurations.
// It checks for environment variables that start with "MULTISTAGE_PARAM_OVERRIDE_" and applies them to the environment settings of each test.
func applyEnvOverrides(o *options) {
	for _, envVar := range os.Environ() {
		if !strings.HasPrefix(envVar, "MULTISTAGE_PARAM_OVERRIDE_") {
			continue
		}
		parts := strings.SplitN(envVar, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, value := parts[0], parts[1]
		for _, test := range o.configSpec.Tests {
			if test.MultiStageTestConfigurationLiteral != nil {
				if test.MultiStageTestConfigurationLiteral.Environment == nil {
					test.MultiStageTestConfigurationLiteral.Environment = make(api.TestEnvironment)
				}
				test.MultiStageTestConfigurationLiteral.Environment[key] = value
			}
		}
	}
}

func overrideTestStepDependencyParams(o *options) error {
	dependencyOverrideParams, err := parseKeyValParams(o.dependencyOverrides.values, "dependency-override-param")

	if err != nil {
		return err
	}

	// first see if there are any dependency overrides at the test level. This should really only happen with rehearsals.
	for _, test := range o.configSpec.Tests {
		if test.MultiStageTestConfigurationLiteral != nil {
			for dependencyName, pullspec := range test.MultiStageTestConfigurationLiteral.DependencyOverrides {
				overrideTestStepDependency(dependencyName, pullspec, &test.MultiStageTestConfigurationLiteral.Pre)
				overrideTestStepDependency(dependencyName, pullspec, &test.MultiStageTestConfigurationLiteral.Test)
				overrideTestStepDependency(dependencyName, pullspec, &test.MultiStageTestConfigurationLiteral.Post)
			}
		}
	}

	// dependency overrides specified as params to ci-operator always take precedence.
	for dependencyName, pullspec := range dependencyOverrideParams {
		for _, test := range o.configSpec.Tests {
			if test.MultiStageTestConfigurationLiteral != nil {
				overrideTestStepDependency(dependencyName, pullspec, &test.MultiStageTestConfigurationLiteral.Pre)
				overrideTestStepDependency(dependencyName, pullspec, &test.MultiStageTestConfigurationLiteral.Test)
				overrideTestStepDependency(dependencyName, pullspec, &test.MultiStageTestConfigurationLiteral.Post)
			}
		}
	}

	return nil
}

func overrideTestStepDependency(name string, value string, steps *[]api.LiteralTestStep) {
	for stepI, step := range *steps {
		for depI, dependency := range step.Dependencies {
			if strings.EqualFold(dependency.Env, name) {
				steps := *steps
				steps[stepI].Dependencies[depI].PullSpec = value
			}
		}
	}
}

func excludeContextCancelledErrors(errs []error) []error {
	var ret []error
	for _, err := range errs {
		if !errors.Is(err, context.Canceled) {
			ret = append(ret, err)
		}
	}
	return ret
}

func (o *options) Report(errs ...error) {
	if len(errs) > 0 {
		o.writeFailingJUnit(errs)
	}

	reporter, loadErr := o.resultsOptions.Reporter(o.jobSpec, o.consoleHost)
	if loadErr != nil {
		logrus.WithError(loadErr).Warn("Could not load result reporting options.")
		return
	}

	errorToReport := excludeContextCancelledErrors(errs)
	for _, err := range errorToReport {
		reporter.Report(err)
	}

	if len(errorToReport) == 0 {
		reporter.Report(nil)
	}
}

func (o *options) Run() []error {
	start := time.Now()
	defer func() {
		logrus.Infof("Ran for %s", time.Since(start).Truncate(time.Second))
	}()
	ctx, cancel := context.WithCancel(context.Background())
	handler := func(s os.Signal) {
		logrus.Infof("error: Process interrupted with signal %s, cancelling execution...", s)
		cancel()
	}
	var leaseClient *lease.Client
	if o.leaseServer != "" && o.leaseServerCredentialsFile != "" {
		leaseClient = &o.leaseClient
	}

	o.resolveConsoleHost()

	streams, err := integratedStreams(o.configSpec, o.resolverClient, o.clusterConfig)
	if err != nil {
		return []error{results.ForReason("config_resolver").WithError(err).Errorf("failed to generate integrated streams: %v", err)}
	}

	client, err := coreclientset.NewForConfig(o.clusterConfig)
	if err != nil {
		return []error{fmt.Errorf("could not get core client for cluster config: %w", err)}
	}

	nodeArchitectures, err := resolveNodeArchitectures(ctx, client.Nodes())
	if err != nil {
		return []error{fmt.Errorf("could not resolve the node architectures: %w", err)}
	}

	injectedTest := o.injectTest != ""
	// load the graph from the configuration
	buildSteps, promotionSteps, err := defaults.FromConfig(ctx, o.configSpec, &o.graphConfig, o.jobSpec, o.templates, o.writeParams, o.promote, o.clusterConfig,
		o.podPendingTimeout, leaseClient, o.targets.values, o.cloneAuthConfig, o.pullSecret, o.pushSecret, o.censor, o.hiveKubeconfig,
		o.nodeName, nodeArchitectures, o.targetAdditionalSuffix, o.manifestToolDockerCfg, o.localRegistryDNS, streams, injectedTest, o.enableSecretsStoreCSIDriver, o.metricsAgent)
	if err != nil {
		return []error{results.ForReason("defaulting_config").WithError(err).Errorf("failed to generate steps from config: %v", err)}
	}

	// Before we create the namespace, we need to ensure all inputs to the graph
	// have been resolved. We must run this step before we resolve the partial
	// graph or otherwise two jobs with different targets would create different
	// artifact caches.
	if err := o.resolveInputs(buildSteps); err != nil {
		return []error{results.ForReason("resolving_inputs").WithError(err).Errorf("could not resolve inputs: %v", err)}
	}

	if err := o.writeMetadataJSON(); err != nil {
		return []error{fmt.Errorf("unable to write metadata.json for build: %w", err)}
	}
	// convert the full graph into the subset we must run
	nodes, err := api.BuildPartialGraph(buildSteps, o.targets.values)
	if err != nil {
		return []error{results.ForReason("building_graph").WithError(err).Errorf("could not build execution graph: %v", err)}
	}

	// Resolve which of the steps should enable multi arch based on the graph build steps.
	api.ResolveMultiArch(nodes)

	stepList, errs := nodes.TopologicalSort()
	if errs != nil {
		return append([]error{results.ForReason("building_graph").ForError(errors.New("could not sort nodes"))}, errs...)
	}
	logrus.Infof("Running %s", strings.Join(nodeNames(stepList), ", "))
	if o.printGraph {
		if err := printDigraph(os.Stdout, stepList); err != nil {
			return []error{fmt.Errorf("could not print graph: %w", err)}
		}
		return nil
	}
	graph, errs := calculateGraph(stepList)
	if errs != nil {
		return errs
	}
	defer func() {
		serializedGraph, err := json.Marshal(graph)
		if err != nil {
			logrus.WithError(err).Error("Failed to marshal graph")
			return
		}

		_ = api.SaveArtifact(o.censor, api.CIOperatorStepGraphJSONFilename, serializedGraph)
	}()
	// initialize the namespace if necessary and create any resources that must
	// exist prior to execution
	if err := o.initializeNamespace(); err != nil {
		return []error{results.ForReason("initializing_namespace").WithError(err).Errorf("could not initialize namespace: %v", err)}
	}
	o.metricsAgent.Record(&metrics.InsightsEvent{Name: "namespace_created", AdditionalContext: map[string]any{"namespace": o.namespace}})

	return interrupt.New(handler, o.saveNamespaceArtifacts).Run(func() []error {
		if leaseClient != nil {
			if err := o.initializeLeaseClient(); err != nil {
				return []error{fmt.Errorf("failed to create the lease client: %w", err)}
			}
		}
		go monitorNamespace(ctx, cancel, o.namespace, client.Namespaces())
		authClient, err := authclientset.NewForConfig(o.clusterConfig)
		if err != nil {
			return []error{fmt.Errorf("could not get auth client for cluster config: %w", err)}
		}
		eventRecorder, err := eventRecorder(client, authClient, o.namespace)
		if err != nil {
			return []error{fmt.Errorf("could not create event recorder: %w", err)}
		}
		runtimeObject := &coreapi.ObjectReference{Namespace: o.namespace}
		eventRecorder.Event(runtimeObject, coreapi.EventTypeNormal, "CiJobStarted", eventJobDescription(o.jobSpec, o.namespace))
		// execute the graph
		suites, graphDetails, errs := steps.Run(ctx, nodes)
		if err := o.writeJUnit(suites, "operator"); err != nil {
			logrus.WithError(err).Warn("Unable to write JUnit result.")
		}
		graph.MergeFrom(graphDetails...)
		// Rewrite the Metadata JSON to catch custom metadata if it has been generated by the job
		if err := o.writeMetadataJSON(); err != nil {
			logrus.WithError(err).Warn("Unable to update metadata.json for build")
		}
		if len(errs) > 0 {
			o.metricsAgent.Record(&metrics.InsightsEvent{Name: "ci_operator_failed"})
			eventRecorder.Event(runtimeObject, coreapi.EventTypeWarning, "CiJobFailed", eventJobDescription(o.jobSpec, o.namespace))
			var wrapped []error
			for _, err := range errs {
				wrapped = append(wrapped, &errWroteJUnit{wrapped: results.ForReason("executing_graph").WithError(err).Errorf("could not run steps: %v", err)})
			}
			return wrapped
		}

		// Run each of the promotion steps concurrently
		lenOfPromotionSteps := len(promotionSteps)
		detailsChan := make(chan api.CIOperatorStepDetails, lenOfPromotionSteps)
		errChan := make(chan error, lenOfPromotionSteps)
		for _, step := range promotionSteps {
			go runPromotionStep(ctx, step, detailsChan, errChan)
		}
		for i := 0; i < lenOfPromotionSteps; i++ {
			select {
			case details := <-detailsChan:
				graph.MergeFrom(details)
			case err := <-errChan:
				errorDesc := fmt.Sprintf("post step failed while %s. with error: %v", eventJobDescription(o.jobSpec, o.namespace), err)
				eventRecorder.Event(runtimeObject, coreapi.EventTypeWarning, "PostStepFailed", errorDesc)
				o.metricsAgent.Record(&metrics.InsightsEvent{Name: "post_step_failed", AdditionalContext: map[string]any{"error": errorDesc}})
				return []error{results.ForReason("executing_post").WithError(err).Unwrap()} // If any of the promotion steps fail, it is considered a failure
			}
		}

		eventRecorder.Event(runtimeObject, coreapi.EventTypeNormal, "CiJobSucceeded", eventJobDescription(o.jobSpec, o.namespace))
		o.metricsAgent.Record(&metrics.InsightsEvent{Name: "ci_operator_succeeded"})
		return nil
	})
}

func runPromotionStep(ctx context.Context, step api.Step, detailsChan chan<- api.CIOperatorStepDetails, errChan chan<- error) {
	details, err := runStep(ctx, step)
	if err != nil {
		errChan <- fmt.Errorf("could not run promotion step %s: %w", step.Name(), err)
	} else {
		detailsChan <- details
	}
}

func integratedStreams(config *api.ReleaseBuildConfiguration, client server.ResolverClient, clusterConfig *rest.Config) (map[string]*configresolver.IntegratedStream, error) {
	if config == nil {
		return nil, errors.New("unable to get integrated stream for nil config")
	}
	if client == nil {
		return nil, errors.New("unable to get integrated stream with nil client")
	}
	if clusterConfig == nil {
		return nil, errors.New("unable to get integrated stream with nil rest config")
	}
	ocClient, err := ctrlruntimeclient.New(clusterConfig, ctrlruntimeclient.Options{})
	if err != nil {
		return nil, fmt.Errorf("unable to create oc client to get integreated streams: %w", err)
	}
	ret := map[string]*configresolver.IntegratedStream{}
	var objectKeys []ctrlruntimeclient.ObjectKey
	if config.ReleaseTagConfiguration != nil {
		objectKeys = append(objectKeys, ctrlruntimeclient.ObjectKey{Namespace: config.ReleaseTagConfiguration.Namespace, Name: config.ReleaseTagConfiguration.Name})
	}
	for _, release := range config.Releases {
		if release.Integration != nil {
			objectKeys = append(objectKeys, ctrlruntimeclient.ObjectKey{Namespace: release.Integration.Namespace, Name: release.Integration.Name})
		}
	}
	for _, key := range objectKeys {
		if api.IsCreatedForClusterBotJob(key.Namespace) {
			stream, err := configresolver.LocalIntegratedStream(context.TODO(), ocClient, key.Namespace, key.Name)
			if err != nil {
				return nil, fmt.Errorf("failed to get integrated stream %s/%s for a cluster bot job: %w", key.Namespace, key.Name, err)
			}
			ret[fmt.Sprintf("%s/%s", key.Namespace, key.Name)] = stream
			continue
		}
		integratedStream, err := client.IntegratedStream(key.Namespace, key.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to get integrated stream %s/%s: %w", key.Namespace, key.Name, err)
		}
		ret[fmt.Sprintf("%s/%s", key.Namespace, key.Name)] = integratedStream
	}
	return ret, nil
}

// runStep mostly duplicates steps.runStep. The latter uses an *api.StepNode though and we only have an api.Step for the PostSteps
// so we can not re-use it.
func runStep(ctx context.Context, step api.Step) (api.CIOperatorStepDetails, error) {
	start := time.Now()
	err := step.Run(ctx)
	duration := time.Since(start)
	failed := err != nil

	var subSteps []api.CIOperatorStepDetailInfo
	if x, ok := step.(steps.SubStepReporter); ok {
		subSteps = x.SubSteps()
	}

	return api.CIOperatorStepDetails{
		CIOperatorStepDetailInfo: api.CIOperatorStepDetailInfo{
			StepName:    step.Name(),
			Description: step.Description(),
			StartedAt:   &start,
			FinishedAt: func() *time.Time {
				ret := start.Add(duration)
				return &ret
			}(),
			Duration: &duration,
			Failed:   &failed,
		},
		Substeps: subSteps,
	}, err
}

func (o *options) resolveConsoleHost() {
	if client, err := ctrlruntimeclient.New(o.clusterConfig, ctrlruntimeclient.Options{}); err != nil {
		logrus.WithError(err).Warn("Could not create client for accessing Routes. Will not resolve console URL.")
	} else {
		host, err := api.ResolveConsoleHost(context.TODO(), client)
		if err != nil {
			logrus.WithError(err).Warn("Could not resolve OpenShift console host. Will not resolve console URL.")
		} else {
			o.consoleHost = host
		}
	}
}

func (o *options) resolveInputs(steps []api.Step) error {
	var inputs api.InputDefinition
	for _, step := range steps {
		definition, err := step.Inputs()
		if err != nil {
			return fmt.Errorf("could not determine inputs for step %s: %w", step.Name(), err)
		}
		inputs = append(inputs, definition...)
	}

	// a change in the config for the build changes the output
	cs := o.configSpec
	// The targetAdditionalSuffix should be trimmed for the input purposes as the intent is to have the different suffix resolve the same
	targetAdditionalSuffix := o.targetAdditionalSuffix
	if targetAdditionalSuffix != "" {
		cs = o.configSpec.DeepCopy()
		for i, test := range cs.Tests {
			for _, target := range o.targets.values {
				if test.As == target {
					suffix := fmt.Sprintf("-%s", targetAdditionalSuffix)
					logrus.Debugf("Trimming suffix: %s from: %s for input resolution", suffix, target)
					cs.Tests[i].As = strings.TrimSuffix(test.As, suffix)
					break
				}
			}
		}
	}
	configSpec, err := yaml.Marshal(cs)
	if err != nil {
		panic(err)
	}
	inputs = append(inputs, string(configSpec))
	if len(o.extraInputHash.values) > 0 {
		inputs = append(inputs, o.extraInputHash.values...)
	}

	// add the binary modification time and size (in lieu of a content hash)
	path, _ := exec.LookPath(os.Args[0])
	if len(path) == 0 {
		path = os.Args[0]
	}
	if stat, err := os.Stat(path); err == nil {
		logrus.Tracef("Using binary as hash: %s %d %d", path, stat.ModTime().UTC().Unix(), stat.Size())
		inputs = append(inputs, fmt.Sprintf("%d-%d", stat.ModTime().UTC().Unix(), stat.Size()))
	} else {
		logrus.WithError(err).Trace("Could not calculate info from current binary to add to input hash.")
	}

	sort.Strings(inputs)
	o.inputHash = inputHash(inputs)

	// input hash is unique for a given job definition and input refs
	if len(o.namespace) == 0 {
		o.namespace = "ci-op-{id}"
	}
	o.namespace = strings.Replace(o.namespace, "{id}", o.inputHash, -1)
	// TODO: instead of mutating this here, we should pass the parts of graph execution that are resolved
	// after the graph is created but before it is run down into the run step.
	o.jobSpec.SetNamespace(o.namespace)

	// If we can resolve the field, use it. If not, don't.
	if o.consoleHost != "" {
		logrus.Infof("Using namespace https://%s/k8s/cluster/projects/%s", o.consoleHost, o.namespace)
	} else {
		logrus.Infof("Using namespace %s", o.namespace)
	}

	return nil
}

func (o *options) initializeNamespace() error {
	// We have to keep the project client because it return a project for a projectCreationRequest, ctrlruntimeclient can not do dark magic like that
	projectGetter, err := projectclientset.NewForConfig(o.clusterConfig)
	if err != nil {
		return fmt.Errorf("could not get project client for cluster config: %w", err)
	}
	ctrlClient, err := ctrlruntimeclient.New(o.clusterConfig, ctrlruntimeclient.Options{})
	if err != nil {
		return fmt.Errorf("failed to construct client: %w", err)
	}
	client := ctrlruntimeclient.NewNamespacedClient(ctrlClient, o.namespace)
	client = labeledclient.Wrap(client, o.jobSpec)
	ctx := context.Background()

	logrus.Debugf("Creating namespace %s", o.namespace)
	authTimeout := 15 * time.Second
	initBeginning := time.Now()
	for {
		project, err := projectGetter.ProjectV1().ProjectRequests().Create(context.TODO(), &projectapi.ProjectRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:   o.namespace,
				Labels: map[string]string{api.DPTPRequesterLabel: "ci-operator"},
			},
			DisplayName: fmt.Sprintf("%s - %s", o.namespace, o.jobSpec.Job),
			Description: jobDescription(o.jobSpec),
		}, metav1.CreateOptions{})
		if err != nil && !kerrors.IsAlreadyExists(err) {
			return fmt.Errorf("could not set up namespace for test: %w", err)
		}
		if err != nil {
			project, err = projectGetter.ProjectV1().Projects().Get(context.TODO(), o.namespace, metav1.GetOptions{})
			if err != nil {
				if kerrors.IsNotFound(err) {
					continue
				}
				// wait a few seconds for auth caches to catch up
				if kerrors.IsForbidden(err) && time.Since(initBeginning) < authTimeout {
					time.Sleep(time.Second)
					continue
				}
				return fmt.Errorf("failed to wait for authentication cache to warm up after %s: %w", authTimeout, err)
			}
		}
		if project.Status.Phase == coreapi.NamespaceTerminating {
			logrus.Info("Waiting for namespace to finish terminating before creating another")
			time.Sleep(3 * time.Second)
			continue
		}
		break
	}

	ssarStart := time.Now()
	var selfSubjectAccessReviewSucceeded bool
	for i := 0; i < 30; i++ {
		sar := &authapi.SelfSubjectAccessReview{Spec: authapi.SelfSubjectAccessReviewSpec{ResourceAttributes: &authapi.ResourceAttributes{
			Namespace: o.namespace,
			Verb:      "create",
			Resource:  "rolebindings",
		}}}
		if err := client.Create(ctx, sar); err != nil {
			logrus.WithError(err).Warn("Failed to create SelfSubjectAccessReview when checking to see if the namespace was initialized.")
			continue
		}
		if sar.Status.Allowed {
			selfSubjectAccessReviewSucceeded = true
			break
		}
		logrus.Debugf("[%d/30] RBAC in namespace not yet ready, sleeping for a second...", i)
		time.Sleep(time.Second)
	}
	logrus.Debugf("Spent %v waiting for RBAC to initialize in the new namespace.", time.Since(ssarStart))
	if !selfSubjectAccessReviewSucceeded {
		logrus.Error("Timed out waiting for RBAC to initialize in the test namespace.")
		return errors.New("timed out waiting for RBAC")
	}

	// Annotate the namespace for cleanup by external tooling (ci-ns-ttl-controller)
	// Unfortunately we cannot set the annotations right away when we create a project
	// because that API does not support it (historical limitation).
	//
	// We can also only annotate the project *after* the SSAR check above, which
	// means that if SSAR fails, the project will *not* be annotated for cleanup.
	annotationUpdates := map[string]string{}
	if o.idleCleanupDuration > 0 {
		if o.idleCleanupDurationSet {
			logrus.Debugf("Setting a soft TTL of %s for the namespace", o.idleCleanupDuration.String())
		}
		annotationUpdates[nsttl.AnnotationIdleCleanupDurationTTL] = o.idleCleanupDuration.String()
	}

	if o.cleanupDuration > 0 {
		if o.cleanupDurationSet {
			logrus.Debugf("Setting a hard TTL of %s for the namespace", o.cleanupDuration.String())
		}
		annotationUpdates[nsttl.AnnotationCleanupDurationTTL] = o.cleanupDuration.String()
	}

	// This label makes sure that the namespace is active, and the value will be updated
	// if the namespace will be reused.
	annotationUpdates[nsttl.AnnotationNamespaceLastActive] = time.Now().Format(time.RFC3339)

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		ns := &coreapi.Namespace{}
		if err := client.Get(ctx, ctrlruntimeclient.ObjectKey{Name: o.namespace}, ns); err != nil {
			return err
		}
		ns.Labels = steps.LabelsFor(o.jobSpec, map[string]string{api.AutoScalePodsLabel: "true"}, "")

		if ns.Annotations == nil {
			ns.Annotations = make(map[string]string)
		}
		for key, value := range annotationUpdates {
			// allow specific annotations to be skipped if they are already set and the user didn't ask
			switch key {
			case nsttl.AnnotationCleanupDurationTTL:
				if !o.cleanupDurationSet && len(ns.Annotations[key]) != 0 {
					continue
				}
			case nsttl.AnnotationIdleCleanupDurationTTL:
				if !o.idleCleanupDurationSet && len(ns.Annotations[key]) != 0 {
					continue
				}
			}
			ns.ObjectMeta.Annotations[key] = value
		}

		updateErr := client.Update(ctx, ns)
		if kerrors.IsForbidden(updateErr) {
			logrus.WithError(err).Warn("Could not edit namespace because you do not have permission to update the namespace.")
			return nil
		}
		return updateErr
	}); err != nil {
		return fmt.Errorf("could not update namespace to add labels, TTLs and active annotations: %w", err)
	}

	intranetAccess := false
	for _, test := range o.configSpec.Tests {
		if slices.Contains(o.targets.values, test.As) {
			if test.RestrictNetworkAccess != nil && !*test.RestrictNetworkAccess {
				intranetAccess = true
				break
			}
		}
	}

	if intranetAccess {
		logrus.Debugf("Deleting egress firewall in namespace %s", o.namespace)
		egressFirewall := &egressfirewallv1.EgressFirewall{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "default",
				Namespace: o.namespace,
			},
		}
		if err := client.Delete(ctx, egressFirewall); err != nil {
			if kerrors.IsNotFound(err) {
				logrus.Warnf("Warning: egress firewall does not exist: %v", err)
			} else {
				if errors.Is(err, &meta.NoKindMatchError{}) {
					logrus.Warnf("crd not installed: %s", err)
				} else {
					return fmt.Errorf("could not delete egress firewall: %w", err)
				}
			}
		}
	}

	pullStart := time.Now()
	var imagePullSecretsMinted bool
	for i := 0; i < 299; i++ {
		imagePullSecretsMinted = true
		serviceAccounts := map[string]*coreapi.ServiceAccount{
			"builder": {},
			"default": {},
		}
		for name := range serviceAccounts {
			if err := client.Get(ctx, ctrlruntimeclient.ObjectKey{Namespace: o.namespace, Name: name}, serviceAccounts[name]); err != nil && !kerrors.IsNotFound(err) {
				return fmt.Errorf("failed to fetch service account %s: %w", name, err)
			}
			imagePullSecretsMinted = imagePullSecretsMinted && len(serviceAccounts[name].ImagePullSecrets) > 0
		}
		if imagePullSecretsMinted {
			break
		}
		logrus.Debugf("[%d/300] Image pull secrets in namespace not yet ready, sleeping for a second...", i)
		time.Sleep(time.Second)
	}
	logrus.Debugf("Spent %v waiting for image pull secrets to initialize in the new namespace.", time.Since(pullStart))
	if !imagePullSecretsMinted {
		logrus.Error("Timed out waiting for image pull secrets in the test namespace.")
		return errors.New("timed out waiting for image pull secrets")
	}

	if o.givePrAuthorAccessToNamespace && len(o.authors) > 0 {
		roleBinding := generateAuthorAccessRoleBinding(o.namespace, o.authors)
		// Generate rolebinding for all the PR Authors.
		logrus.WithField("authors", o.authors).Debugf("Creating ci-op-author-access rolebinding in namespace %s", o.namespace)
		if err := client.Create(ctx, roleBinding); err != nil && !kerrors.IsAlreadyExists(err) {
			return fmt.Errorf("could not create role binding for: %w", err)
		}

	}

	for _, secret := range []*coreapi.Secret{o.pullSecret, o.pushSecret, o.uploadSecret} {
		if secret != nil {
			secret.Immutable = utilpointer.Bool(true)
			if err := client.Create(ctx, secret); err != nil && !kerrors.IsAlreadyExists(err) {
				return fmt.Errorf("couldn't create secret %s: %w", secret.Name, err)
			}
		}
	}

	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ns := &coreapi.Namespace{}
				if err := client.Get(ctx, ctrlruntimeclient.ObjectKey{Name: o.namespace}, ns); err != nil {
					logrus.WithError(err).Warnf("Failed to get namespace %s for heartbeating", o.namespace)
					continue
				}
				originalNS := ns.DeepCopy()
				if ns.Annotations == nil {
					ns.Annotations = map[string]string{}
				}
				ns.Annotations[nsttl.AnnotationNamespaceLastActive] = time.Now().Format(time.RFC3339)
				if err := client.Patch(ctx, ns, ctrlruntimeclient.MergeFrom(originalNS)); err != nil {
					logrus.WithError(err).Warnf("Failed to patch the %s namespace to update the %s annotation.", o.namespace, nsttl.AnnotationNamespaceLastActive)
				}
			}
		}
	}()

	logrus.Debugf("Setting up pipeline ImageStream for the test")

	// create the image stream or read it to get its uid
	is := &imageapi.ImageStream{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: o.jobSpec.Namespace(),
			Name:      api.PipelineImageStream,
		},
		Spec: imageapi.ImageStreamSpec{
			// pipeline:* will now be directly referenceable
			LookupPolicy: imageapi.ImageLookupPolicy{Local: true},
		},
	}
	if err := client.Create(ctx, is); err != nil {
		if !kerrors.IsAlreadyExists(err) {
			return fmt.Errorf("could not set up pipeline imagestream for test: %w", err)
		}
		if err := client.Get(ctx, ctrlruntimeclient.ObjectKey{Name: api.PipelineImageStream}, is); err != nil {
			return fmt.Errorf("failed to get pipeline imagestream: %w", err)
		}
	}
	o.jobSpec.SetOwner(&metav1.OwnerReference{
		APIVersion: "image.openshift.io/v1",
		Kind:       "ImageStream",
		Name:       api.PipelineImageStream,
		UID:        is.UID,
	})

	if o.cloneAuthConfig != nil && o.cloneAuthConfig.Secret != nil {
		o.cloneAuthConfig.Secret.Immutable = utilpointer.Bool(true)
		if err := client.Create(ctx, o.cloneAuthConfig.Secret); err != nil && !kerrors.IsAlreadyExists(err) {
			return fmt.Errorf("couldn't create secret %s for %s authentication: %w", o.cloneAuthConfig.Secret.Name, o.cloneAuthConfig.Type, err)
		}
	}

	// adds the appropriate cluster profile secrets to o.secrets,
	// so they can be created by ctrlruntime client in the for cycle below this one
	for _, cp := range o.clusterProfiles {
		cpSecret, err := getClusterProfileSecret(cp, labeledclient.Wrap(ctrlClient, o.jobSpec), o.resolverClient, ctx)
		if err != nil {
			return fmt.Errorf("failed to create cluster profile secret %s: %w", cp, err)
		}
		cpSecret.Namespace = o.namespace
		o.secrets = append(o.secrets, cpSecret)
	}

	if o.configSpec.ExternalImages != nil {
		for _, image := range o.configSpec.ExternalImages {
			if image.PullSecret != "" {
				secret, err := getExternalImagePullSecret(ctx, labeledclient.Wrap(ctrlClient, o.jobSpec), image)
				if err != nil {
					return fmt.Errorf("failed to get external image pull secret: %w", err)
				}
				secret.Namespace = o.namespace
				o.secrets = append(o.secrets, secret)
			}
		}
	}

	for _, secret := range o.secrets {
		created, err := util.UpsertImmutableSecret(ctx, client, secret)
		if err != nil {
			return fmt.Errorf("could not update secret %s: %w", secret.Name, err)
		}
		if created {
			logrus.Debugf("Created secret %s", secret.Name)
		} else {
			logrus.Debugf("Updated secret %s", secret.Name)
		}
	}
	pdb, mutateFn := pdb(steps.CreatedByCILabel, o.namespace)
	if _, err := crcontrollerutil.CreateOrUpdate(ctx, client, pdb, mutateFn); err != nil && !kerrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create pdb for label key %s: %w", steps.CreatedByCILabel, err)
	}
	logrus.Debugf("Created PDB for pods with %s label", steps.CreatedByCILabel)
	return nil
}

func getExternalImagePullSecret(ctx context.Context, client ctrlruntimeclient.Client, image api.ExternalImage) (*coreapi.Secret, error) {
	ciSecret := &coreapi.Secret{}
	err := client.Get(ctx, ctrlruntimeclient.ObjectKey{Namespace: "test-credentials", Name: image.PullSecret}, ciSecret)
	if err != nil {
		return nil, fmt.Errorf("failed to get secret '%s' from test-credentials namespace: %w", image.PullSecret, err)
	}

	newSecret := &coreapi.Secret{
		Data: ciSecret.Data,
		Type: coreapi.SecretTypeDockerConfigJson,
		ObjectMeta: metav1.ObjectMeta{
			Name: "external-pull-secret-" + image.PullSecret,
		},
	}
	return newSecret, nil
}

func generateAuthorAccessRoleBinding(namespace string, authors []string) *rbacapi.RoleBinding {
	var subjects []rbacapi.Subject
	authorSet := sets.New[string](authors...)
	for _, author := range sets.List(authorSet) {
		subjects = append(subjects, rbacapi.Subject{Kind: "Group", Name: api.GitHubUserGroup(author)})
	}
	return &rbacapi.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ci-op-author-access",
			Namespace: namespace,
		},
		Subjects: subjects,
		RoleRef: rbacapi.RoleRef{
			Kind: "ClusterRole",
			Name: "admin",
		},
	}
}

func pdb(labelKey, namespace string) (*policyv1.PodDisruptionBudget, crcontrollerutil.MutateFn) {
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("ci-operator-%s", strings.ReplaceAll(labelKey, "/", "-")),
			Namespace: namespace,
		},
	}
	return pdb, func() error {
		pdb.Spec.MaxUnavailable = &intstr.IntOrString{
			Type:   intstr.Int,
			IntVal: 0,
		}
		pdb.Spec.Selector = &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      labelKey,
				Operator: metav1.LabelSelectorOpExists,
			}},
		}
		return nil
	}
}

// prowResultMetadata is the set of metadata consumed by testgrid and
// gubernator after a CI run completes. We add work-namespace as our
// target namespace for the job.
//
// Example from k8s:
//
//	"metadata": {
//		"repo-commit": "253f03e0055b6649f8b25e84122748d39a284141",
//		"node_os_image": "cos-stable-65-10323-64-0",
//		"repos": {
//			"k8s.io/kubernetes": "master:1c04caa04325e1f64d9a15714ad61acdd2a81013,71936:353a0b391d6cb0c26e1c0c6b180b300f64039e0e",
//			"k8s.io/release": "master"
//		},
//		"infra-commit": "de7741746",
//		"repo": "k8s.io/kubernetes",
//		"master_os_image": "cos-stable-65-10323-64-0",
//		"job-version": "v1.14.0-alpha.0.1012+253f03e0055b66",
//		"pod": "dd8d320f-ff64-11e8-b091-0a580a6c02ef"
//	}
type prowResultMetadata struct {
	Revision      string            `json:"revision"`
	RepoCommit    string            `json:"repo-commit"`
	Repo          string            `json:"repo"`
	Repos         map[string]string `json:"repos"`
	InfraCommit   string            `json:"infra-commit"`
	JobVersion    string            `json:"job-version"`
	Pod           string            `json:"pod"`
	WorkNamespace string            `json:"work-namespace"`
	Metadata      map[string]string `json:"metadata"`
}

const metadataJSONfile = "metadata.json"

func (o *options) writeMetadataJSON() error {
	artifactDir, set := api.Artifacts()
	if !set {
		return nil
	}

	metadataJSONPath := filepath.Join(artifactDir, metadataJSONfile)

	customProwMetadataFile, err := o.findCustomMetadataFile(artifactDir)

	if err != nil {
		logrus.WithError(err).Error("Could not find custom prow metadata file.")
		return err
	}

	// If the metadata JSON exists and there's no custom prow metadata, then skip the second write.
	_, err = os.Stat(metadataJSONPath)
	if customProwMetadataFile == "" && err == nil {
		logrus.Debug("No custom metadata found and prow metadata already exists. Not updating the metadata.")
		return nil
	}

	var customMetadataErr error

	m := o.generateProwMetadata()
	if customProwMetadataFile != "" {
		m.Metadata, customMetadataErr = o.parseCustomMetadata(customProwMetadataFile)
	}

	if customMetadataErr != nil {
		logrus.WithError(err).Warn("Error parsing custom metadata.")
	}

	data, _ := json.MarshalIndent(m, "", "")
	err = api.SaveArtifact(o.censor, metadataJSONfile, data)

	if err != nil {
		return err
	} else if customMetadataErr != nil {
		return customMetadataErr
	}

	return nil
}

func (o *options) findCustomMetadataFile(artifactDir string) (customProwMetadataFile string, err error) {
	// Try to find the custom prow metadata file. We assume that there's only one. If there's more than one,
	// we'll just use the first one that we find.
	err = filepath.WalkDir(artifactDir, func(path string, info fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		if info.Name() == CustomProwMetadata {
			if customProwMetadataFile == "" {
				customProwMetadataFile = path
			} else {
				logrus.Error("Multiple custom prow metadata files found, which are not currently supported by ci-operator.")
			}
			return filepath.SkipDir
		}

		return nil
	})
	return customProwMetadataFile, err
}

// generateProwMetadata generates the normal prow metadata from the arguments passed into ci-operator
func (o *options) generateProwMetadata() (m prowResultMetadata) {
	o.metadataRevision++
	m.Revision = strconv.Itoa(o.metadataRevision)

	if o.jobSpec.Refs != nil {
		m.Repo = fmt.Sprintf("%s/%s", o.jobSpec.Refs.Org, o.jobSpec.Refs.Repo)
		m.Repos = map[string]string{m.Repo: o.jobSpec.Refs.String()}
	}
	if len(o.jobSpec.ExtraRefs) > 0 {
		if m.Repos == nil {
			m.Repos = make(map[string]string)
		}
		for _, ref := range o.jobSpec.ExtraRefs {
			repo := fmt.Sprintf("%s/%s", ref.Org, ref.Repo)
			if _, ok := m.Repos[repo]; ok {
				continue
			}
			m.Repos[repo] = ref.String()
		}
	}

	m.Pod = o.jobSpec.ProwJobID
	m.WorkNamespace = o.namespace

	return m
}

// parseCustomMetadata parses metadata from the custom prow metadata file
func (o *options) parseCustomMetadata(customProwMetadataFile string) (customMetadata map[string]string, err error) {
	logrus.Info("Found custom prow metadata.")

	if customJSONFile, readingError := os.ReadFile(customProwMetadataFile); readingError != nil {
		logrus.WithError(readingError).Error("Failed to read custom prow metadata.")
	} else {
		err = json.Unmarshal(customJSONFile, &customMetadata)
		if err != nil {
			logrus.WithError(err).Error("Failed to unmarshal custom prow metadata.")
		}
	}

	censoredMetadata := map[string]string{}
	for key, value := range customMetadata {
		rawKey, rawValue := []byte(key), []byte(value)
		o.censor.Censor(&rawKey)
		o.censor.Censor(&rawValue)
		censoredMetadata[string(rawKey)] = string(rawValue)
	}

	return censoredMetadata, err
}

// errWroteJUnit indicates that this error is covered by existing JUnit output and writing
// another JUnit file is not necessary (in writeFailingJUnit)
type errWroteJUnit struct {
	wrapped error
}

// Error makes an errWroteJUnit an error
func (e *errWroteJUnit) Error() string {
	return e.wrapped.Error()
}

// Unwrap allows nesting of errors
func (e *errWroteJUnit) Unwrap() error {
	return e.wrapped
}

// Is allows us to say we are an errWroteJUnit
func (e *errWroteJUnit) Is(target error) bool {
	_, is := target.(*errWroteJUnit)
	return is
}

func sortSuite(suite *junit.TestSuite) {
	sort.Slice(suite.Properties, func(i, j int) bool {
		return suite.Properties[i].Name < suite.Properties[j].Name
	})
	sort.Slice(suite.Children, func(i, j int) bool {
		return suite.Children[i].Name < suite.Children[j].Name
	})
	sort.Slice(suite.TestCases, func(i, j int) bool {
		return suite.TestCases[i].Name < suite.TestCases[j].Name
	})
	for i := range suite.Children {
		sortSuite(suite.Children[i])
	}
}

// writeFailingJUnit attempts to write a JUnit artifact when the graph could not be
// initialized in order to capture the result for higher level automation.
func (o *options) writeFailingJUnit(errs []error) {
	var testCases []*junit.TestCase
	for _, err := range errs {
		if errors.Is(err, &errWroteJUnit{}) {
			continue
		}
		testCases = append(testCases, &junit.TestCase{
			Name: "initialize",
			FailureOutput: &junit.FailureOutput{
				Output: err.Error(),
			},
		})
	}
	if len(testCases) == 0 {
		return
	}
	suites := &junit.TestSuites{
		Suites: []*junit.TestSuite{
			{
				Name:      "job",
				NumTests:  uint(len(errs)),
				NumFailed: uint(len(errs)),
				TestCases: testCases,
			},
		},
	}
	if err := o.writeJUnit(suites, "job"); err != nil {
		logrus.Trace("Unable to write top level failing JUnit artifact")
	}
}

func (o *options) writeJUnit(suites *junit.TestSuites, name string) error {
	if suites == nil {
		return nil
	}
	sort.Slice(suites.Suites, func(i, j int) bool {
		return suites.Suites[i].Name < suites.Suites[j].Name
	})
	for i := range suites.Suites {
		junit.CensorTestSuite(o.censor, suites.Suites[i])
		sortSuite(suites.Suites[i])
	}
	out, err := xml.MarshalIndent(suites, "", "  ")
	if err != nil {
		return fmt.Errorf("could not marshal jUnit XML: %w", err)
	}
	return api.SaveArtifact(o.censor, fmt.Sprintf("junit_%s.xml", name), out)
}

// oneWayEncoding can be used to encode hex to a 62-character set (0 and 1 are duplicates) for use in
// short display names that are safe for use in kubernetes as resource names.
var oneWayNameEncoding = base32.NewEncoding("bcdfghijklmnpqrstvwxyz0123456789").WithPadding(base32.NoPadding)

// inputHash returns a string that hashes the unique parts of the input to avoid collisions.
func inputHash(inputs api.InputDefinition) string {
	hash := sha256.New()

	// the inputs form a part of the hash
	for _, s := range inputs {
		if _, err := hash.Write([]byte(s)); err != nil {
			logrus.WithError(err).Error("Failed to write hash.")
		}
	}

	// Object names can't be too long so we truncate
	// the hash. This increases chances of collision
	// but we can tolerate it as our input space is
	// tiny.
	return oneWayNameEncoding.EncodeToString(hash.Sum(nil)[:5])
}

// saveNamespaceArtifacts is a best effort attempt to save ci-operator namespace artifacts to disk
// for review later.
func (o *options) saveNamespaceArtifacts() {
	namespaceDir := api.NamespaceDir
	if kubeClient, err := coreclientset.NewForConfig(o.clusterConfig); err == nil {
		pods, _ := kubeClient.Pods(o.namespace).List(context.TODO(), metav1.ListOptions{})
		data, _ := json.MarshalIndent(pods, "", "  ")
		path := filepath.Join(namespaceDir, "pods.json")
		_ = api.SaveArtifact(o.censor, path, data)
		events, _ := kubeClient.Events(o.namespace).List(context.TODO(), metav1.ListOptions{})
		data, _ = json.MarshalIndent(events, "", "  ")
		path = filepath.Join(namespaceDir, "events.json")
		_ = api.SaveArtifact(o.censor, path, data)
	}

	if buildClient, err := buildclientset.NewForConfig(o.clusterConfig); err == nil {
		builds, _ := buildClient.Builds(o.namespace).List(context.TODO(), metav1.ListOptions{})
		data, _ := json.MarshalIndent(builds, "", "  ")
		path := filepath.Join(namespaceDir, "builds.json")
		_ = api.SaveArtifact(o.censor, path, data)
	}

	if client, err := ctrlruntimeclient.New(o.clusterConfig, ctrlruntimeclient.Options{}); err == nil {
		imagestreams := &imageapi.ImageStreamList{}
		_ = client.List(context.TODO(), imagestreams, ctrlruntimeclient.InNamespace(o.namespace))
		data, _ := json.MarshalIndent(imagestreams, "", "  ")
		path := filepath.Join(namespaceDir, "imagestreams.json")
		_ = api.SaveArtifact(o.censor, path, data)
	}

	if templateClient, err := templateclientset.NewForConfig(o.clusterConfig); err == nil {
		templateInstances, _ := templateClient.TemplateInstances(o.namespace).List(context.TODO(), metav1.ListOptions{})
		data, _ := json.MarshalIndent(templateInstances, "", "  ")
		path := filepath.Join(namespaceDir, "templateinstances.json")
		_ = api.SaveArtifact(o.censor, path, data)
	}
}

func loadLeaseCredentials(leaseServerCredentialsFile string) (string, func() []byte, error) {
	if err := secret.Add(leaseServerCredentialsFile); err != nil {
		return "", nil, fmt.Errorf("failed to start secret agent on file %s: %s", leaseServerCredentialsFile, string(secret.Censor([]byte(err.Error()))))
	}
	splits := strings.Split(string(secret.GetSecret(leaseServerCredentialsFile)), ":")
	if len(splits) != 2 {
		return "", nil, fmt.Errorf("got invalid content of lease server credentials file which must be of the form '<username>:<passwrod>'")
	}
	username := splits[0]
	passwordGetter := func() []byte {
		return []byte(splits[1])
	}
	return username, passwordGetter, nil
}

func (o *options) initializeLeaseClient() error {
	var err error
	owner := o.namespace + "-" + o.jobSpec.UniqueHash()
	username, passwordGetter, err := loadLeaseCredentials(o.leaseServerCredentialsFile)
	if err != nil {
		return fmt.Errorf("failed to load lease credentials: %w", err)
	}
	if o.leaseClient, err = lease.NewClient(owner, o.leaseServer, username, passwordGetter, 60, o.leaseAcquireTimeout); err != nil {
		return fmt.Errorf("failed to create the lease client: %w", err)
	}
	t := time.NewTicker(30 * time.Second)
	go func() {
		for range t.C {
			if err := o.leaseClient.Heartbeat(); err != nil {
				logrus.WithError(err).Warn("Failed to update leases.")
			}
		}
		if l, err := o.leaseClient.ReleaseAll(); err != nil {
			logrus.WithError(err).Errorf("Failed to release leaked leases (%v)", l)
		} else if len(l) != 0 {
			logrus.Warnf("Would leak leases: %v", l)
		}
	}()
	return nil
}

// eventJobDescription returns a string representing the pull requests and authors description, to be used in events.
func eventJobDescription(jobSpec *api.JobSpec, namespace string) string {
	var pulls []string
	var authors []string

	if jobSpec.Refs == nil {
		return fmt.Sprintf("Running job %s in namespace %s", jobSpec.Job, namespace)
	}
	if len(jobSpec.Refs.Pulls) == 1 {
		pull := jobSpec.Refs.Pulls[0]
		return fmt.Sprintf("Running job %s for PR https://github.com/%s/%s/pull/%d in namespace %s from author %s",
			jobSpec.Job, jobSpec.Refs.Org, jobSpec.Refs.Repo, pull.Number, namespace, pull.Author)
	}
	for _, pull := range jobSpec.Refs.Pulls {
		pulls = append(pulls, fmt.Sprintf("https://github.com/%s/%s/pull/%d", jobSpec.Refs.Org, jobSpec.Refs.Repo, pull.Number))
		authors = append(authors, pull.Author)
	}
	return fmt.Sprintf("Running job %s for PRs (%s) in namespace %s from authors (%s)",
		jobSpec.Job, strings.Join(pulls, ", "), namespace, strings.Join(authors, ", "))
}

// jobDescription returns a string representing the job's description.
func jobDescription(job *api.JobSpec) string {
	if job.Refs == nil {
		return job.Job
	}
	var links []string
	for _, pull := range job.Refs.Pulls {
		links = append(links, fmt.Sprintf("https://github.com/%s/%s/pull/%d - %s", job.Refs.Org, job.Refs.Repo, pull.Number, pull.Author))
	}
	if len(links) > 0 {
		return fmt.Sprintf("%s\n\n%s on https://github.com/%s/%s", strings.Join(links, "\n"), job.Job, job.Refs.Org, job.Refs.Repo)
	}
	return fmt.Sprintf("%s on https://github.com/%s/%s ref=%s commit=%s", job.Job, job.Refs.Org, job.Refs.Repo, job.Refs.BaseRef, job.Refs.BaseSHA)
}

func jobSpecFromGitRef(ref string) (*api.JobSpec, error) {
	parts := strings.Split(ref, "@")
	if len(parts) != 2 {
		return nil, fmt.Errorf("must be ORG/NAME@REF")
	}
	prefix := strings.Split(parts[0], "/")
	if len(prefix) != 2 {
		return nil, fmt.Errorf("must be ORG/NAME@REF")
	}
	repo := fmt.Sprintf("https://github.com/%s/%s.git", prefix[0], prefix[1])
	out, err := exec.Command("git", "ls-remote", repo, parts[1]).Output()
	if err != nil {
		return nil, fmt.Errorf("'git ls-remote %s %s' failed with '%w'", repo, parts[1], err)
	}
	resolved := strings.Split(strings.Split(string(out), "\n")[0], "\t")
	sha := resolved[0]
	if len(sha) == 0 {
		return nil, fmt.Errorf("ref '%s' does not point to any commit in '%s'", parts[1], parts[0])
	}
	// sanity check that regular refs are fully determined
	if strings.HasPrefix(resolved[1], "refs/heads/") && !strings.HasPrefix(parts[1], "refs/heads/") {
		if resolved[1] != ("refs/heads/" + parts[1]) {
			trimmed := resolved[1][len("refs/heads/"):]
			// we could fix this for the user, but better to require them to be explicit
			return nil, fmt.Errorf("ref '%s' does not point to any commit in '%s' (did you mean '%s'?)", parts[1], parts[0], trimmed)
		}
	}
	logrus.Infof("Resolved %s to commit %s", ref, sha)
	spec := &api.JobSpec{
		JobSpec: downwardapi.JobSpec{
			Type: prowapi.PeriodicJob,
			Job:  "dev",
			Refs: &prowapi.Refs{
				Org:     prefix[0],
				Repo:    prefix[1],
				BaseRef: parts[1],
				BaseSHA: sha,
			},
		}}
	return spec, nil
}

func nodeNames(nodes []*api.StepNode) []string {
	var names []string
	for _, node := range nodes {
		name := node.Step.Name()
		if len(name) == 0 {
			name = fmt.Sprintf("<%T>", node.Step)
		}
		names = append(names, name)
	}
	return names
}

func printDigraph(w io.Writer, steps api.OrderedStepList) error {
	for i, step := range steps {
		req := step.Step.Requires()
		// Only the first `i` elements can fulfill the requirements since
		// `OrderedStepList` is a topological order.
		for _, other := range steps[:i] {
			if api.HasAnyLinks(req, other.Step.Creates()) {
				if _, err := fmt.Fprintf(w, "%s %s\n", step.Step.Name(), other.Step.Name()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func calculateGraph(nodes api.OrderedStepList) (*api.CIOperatorStepGraph, []error) {
	if err := validateSteps(nodes); err != nil {
		return nil, err
	}
	var result api.CIOperatorStepGraph
	for i, n := range nodes {
		r := api.CIOperatorStepDetails{CIOperatorStepDetailInfo: api.CIOperatorStepDetailInfo{StepName: n.Step.Name(), Description: n.Step.Description()}}
		for _, requirement := range n.Step.Requires() {
			for _, inner := range nodes[:i] {
				if api.HasAnyLinks([]api.StepLink{requirement}, inner.Step.Creates()) {
					r.Dependencies = append(r.Dependencies, inner.Step.Name())
				}
			}
		}
		result = append(result, r)
	}

	return &result, nil
}

func validateSteps(nodes api.OrderedStepList) []error {
	var errs []error
	for _, n := range nodes {
		if err := n.Step.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("step %q failed validation: %w", n.Step.Name(), err))
			if errors.Is(err, steps.NoLeaseClientErr) {
				errs = append(errs, errors.New("a lease client was required but none was provided, add the --lease-... arguments"))
			} else if errors.Is(err, steps.NoHiveClientErr) {
				errs = append(errs, errors.New("a Hive client was required but none was provided, add the --hive-kubeconfig argument"))
			}
		}
	}
	return errs
}

var shaRegex = regexp.MustCompile(`^[0-9a-fA-F]+$`)

// shorten takes a string, and if it looks like a hexadecimal Git SHA it truncates it to
// l characters. The values provided to job spec are not required to be SHAs but could also be
// tags or other git refs.
func shorten(value string, l int) string {
	if len(value) > l && shaRegex.MatchString(value) {
		return value[:l]
	}
	return value
}

func summarizeRef(refs prowapi.Refs) string {
	if len(refs.Pulls) > 0 {
		var pulls []string
		for _, pull := range refs.Pulls {
			pulls = append(pulls, fmt.Sprintf("#%d %s @%s", pull.Number, shorten(pull.SHA, 8), pull.Author))
		}
		return fmt.Sprintf("Resolved source https://github.com/%s/%s to %s@%s, merging: %s", refs.Org, refs.Repo, refs.BaseRef, shorten(refs.BaseSHA, 8), strings.Join(pulls, ", "))
	}
	if refs.BaseSHA == "" {
		return fmt.Sprintf("Resolved SHA missing for %s in https://github.com/%s/%s (will prevent caching)", refs.BaseRef, refs.Org, refs.Repo)
	}
	return fmt.Sprintf("Resolved source https://github.com/%s/%s to %s@%s", refs.Org, refs.Repo, refs.BaseRef, shorten(refs.BaseSHA, 8))
}

func eventRecorder(kubeClient *coreclientset.CoreV1Client, authClient *authclientset.AuthorizationV1Client, namespace string) (record.EventRecorder, error) {
	res, err := authClient.SelfSubjectAccessReviews().Create(context.TODO(), &authapi.SelfSubjectAccessReview{
		Spec: authapi.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authapi.ResourceAttributes{
				Namespace: namespace,
				Verb:      "create",
				Resource:  "events",
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("could not check permission to create events: %w", err)
	}
	if !res.Status.Allowed {
		logrus.Warn("Events will not be created because of lack of permission.")
		return &record.FakeRecorder{}, nil
	}
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&coreclientset.EventSinkImpl{
		Interface: coreclientset.New(kubeClient.RESTClient()).Events("")})
	return eventBroadcaster.NewRecorder(
		templatescheme.Scheme, coreapi.EventSource{Component: namespace}), nil
}

func getCloneSecretFromPath(cloneAuthType steps.CloneAuthType, secretPath string) (*coreapi.Secret, error) {
	secret := &coreapi.Secret{Data: make(map[string][]byte)}
	data, err := os.ReadFile(secretPath)
	if err != nil {
		return nil, fmt.Errorf("could not read file %s for secret: %w", secretPath, err)
	}
	hash := getHashFromBytes(data)
	data = bytes.TrimSpace(data)

	if cloneAuthType == steps.CloneAuthTypeSSH {
		secret.Name = fmt.Sprintf("ssh-%s", hash)
		secret.Type = coreapi.SecretTypeSSHAuth
		// Secret.Data["ssh-privatekey"] is required on SecretTypeSSHAuth type.
		// https://github.com/kubernetes/api/blob/master/core/v1/types.go#L5466-L5470
		secret.Data[coreapi.SSHAuthPrivateKey] = data
	} else if cloneAuthType == steps.CloneAuthTypeOAuth {
		secret.Type = coreapi.SecretTypeBasicAuth
		secret.Name = fmt.Sprintf("oauth-%s", hash)
		secret.Data[steps.OauthSecretKey] = data

		// Those keys will be used in a git source strategy build
		secret.Data["username"] = data
		secret.Data["password"] = data
	}

	return secret, nil
}

func getHashFromBytes(b []byte) string {
	hash := sha256.New()
	if _, err := hash.Write(b); err != nil {
		logrus.WithError(err).Error("Failed to write hash.")
	}
	return oneWayNameEncoding.EncodeToString(hash.Sum(nil)[:5])
}

func getDockerConfigSecret(name, filename string) (*coreapi.Secret, error) {
	src, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("could not read file %s for secret %s: %w", filename, name, err)
	}
	return &coreapi.Secret{
		Data: map[string][]byte{
			coreapi.DockerConfigJsonKey: src,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Type: coreapi.SecretTypeDockerConfigJson,
	}, nil
}

func getSecret(name, filename string) (*coreapi.Secret, error) {
	src, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("could not read file %s for secret %s: %w", filename, name, err)
	}
	return &coreapi.Secret{
		Data: map[string][]byte{
			path.Base(filename): src,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Type: coreapi.SecretTypeOpaque,
	}, nil
}

func resolveGCSCredentialsSecret(jobSpec *api.JobSpec) string {
	if jobSpec.DecorationConfig != nil && jobSpec.DecorationConfig.GCSCredentialsSecret != nil {
		return *jobSpec.DecorationConfig.GCSCredentialsSecret
	}

	return api.GCSUploadCredentialsSecret
}

func (o *options) getResolverInfo(jobSpec *api.JobSpec) *api.Metadata {
	// address and variant can only be set via options
	info := &api.Metadata{Variant: o.variant}

	allRefs := jobSpec.ExtraRefs
	if jobSpec.Refs != nil {
		allRefs = append([]prowapi.Refs{*jobSpec.Refs}, allRefs...)
	}

	// identify org, repo, and branch from refs object
	for _, ref := range allRefs {
		if ref.Org != "" && ref.Repo != "" && ref.BaseRef != "" {
			info.Org += fmt.Sprintf("%s,", ref.Org)
			info.Repo += fmt.Sprintf("%s,", ref.Repo)
			info.Branch += fmt.Sprintf("%s,", ref.BaseRef)
		}
	}
	info.Org = strings.TrimSuffix(info.Org, ",")
	info.Repo = strings.TrimSuffix(info.Repo, ",")
	info.Branch = strings.TrimSuffix(info.Branch, ",")

	// if flags set, override previous values
	if o.org != "" {
		info.Org = o.org
	}
	if o.repo != "" {
		info.Repo = o.repo
	}
	if o.branch != "" {
		info.Branch = o.branch
	}
	return info
}

func (o *options) getInjectTest() (*api.MetadataWithTest, error) {
	if o.injectTest == "" {
		return nil, nil
	}
	var ret api.MetadataWithTest
	if err := json.Unmarshal([]byte(o.injectTest), &ret); err == nil {
		return &ret, nil
	}

	return api.MetadataTestFromString(o.injectTest)
}

type gcsFileReader interface {
	Read(filePath string) ([]byte, error)
}

type bucketReader struct {
	opener prowio.Opener
}

func (b bucketReader) Read(filePath string) ([]byte, error) {
	content, err := prowio.ReadContent(context.Background(), logrus.WithField("file", filePath), b.opener, filePath)
	return content, err
}

const (
	configSpecVar       = "CONFIG_SPEC"
	configSpecGcsUrlVar = "CONFIG_SPEC_GCS_URL"
	unresolvedConfigVar = "UNRESOLVED_CONFIG"
)

// loadConfig loads the standard configuration path, env, gcs bucket env, or configresolver (in that order of priority)
func (o *options) loadConfig(info *api.Metadata, gcsReader gcsFileReader) (*api.ReleaseBuildConfiguration, error) {
	var raw string

	configSpecEnv, configSpecSet := os.LookupEnv(configSpecVar)
	configSpecGCSEnv, configSpecGCSSet := os.LookupEnv(configSpecGcsUrlVar)
	unresolvedConfigEnv, unresolvedConfigSet := os.LookupEnv(unresolvedConfigVar)

	decodeAndUnzip := func(rawConfig string) (string, error) {
		// if being run by pj-rehearse, config spec may be base64 and gzipped
		if decoded, err := base64.StdEncoding.DecodeString(rawConfig); err != nil {
			return rawConfig, nil
		} else {
			data, err := gzip.ReadBytesMaybeGZIP(decoded)
			if err != nil {
				return "", fmt.Errorf("--config error: %w", err)
			}
			return string(data), nil
		}
	}
	switch {
	case len(o.configSpecPath) > 0:
		data, err := gzip.ReadFileMaybeGZIP(o.configSpecPath)
		if err != nil {
			return nil, fmt.Errorf("--config error: %w", err)
		}
		raw = string(data)
	case configSpecSet:
		if len(configSpecEnv) == 0 {
			return nil, fmt.Errorf("%s environment variable cannot be set to an empty string", configSpecVar)
		}
		var err error
		raw, err = decodeAndUnzip(configSpecEnv)
		if err != nil {
			return nil, err
		}
	case configSpecGCSSet:
		if len(configSpecGCSEnv) == 0 {
			return nil, fmt.Errorf("%s environment variable cannot be set to an empty string", configSpecGcsUrlVar)
		}
		content, err := gcsReader.Read(configSpecGCSEnv)
		if err != nil {
			logrus.WithError(err).Fatalf("Error reading %s", configSpecGCSEnv)
		}
		raw, err = decodeAndUnzip(string(content))
		if err != nil {
			return nil, err
		}
	case len(o.unresolvedConfigPath) > 0:
		data, err := gzip.ReadFileMaybeGZIP(o.unresolvedConfigPath)
		if err != nil {
			return nil, fmt.Errorf("--unresolved-config error: %w", err)
		}
		configSpec, err := o.resolverClient.Resolve(data)
		err = results.ForReason("config_resolver_literal").ForError(err)
		return configSpec, err
	case unresolvedConfigSet:
		configSpec, err := o.resolverClient.Resolve([]byte(unresolvedConfigEnv))
		err = results.ForReason("config_resolver_literal").ForError(err)
		return configSpec, err
	default:
		configSpec, err := o.resolverClient.Config(info)
		err = results.ForReason("config_resolver").ForError(err)
		return configSpec, err
	}
	configSpec := api.ReleaseBuildConfiguration{}
	if err := yaml.UnmarshalStrict([]byte(raw), &configSpec); err != nil {
		if len(o.configSpecPath) > 0 {
			return nil, fmt.Errorf("invalid configuration in file %s: %w\nvalue:\n%s", o.configSpecPath, err, raw)
		}
		return nil, fmt.Errorf("invalid configuration: %w\nvalue:\n%s", err, raw)
	}
	if o.registryPath != "" {
		refs, chains, workflows, _, _, _, observers, err := load.Registry(o.registryPath, load.RegistryFlag(0))
		if err != nil {
			return nil, fmt.Errorf("failed to load registry: %w", err)
		}
		configSpec, err = registry.ResolveConfig(registry.NewResolver(refs, chains, workflows, observers), configSpec)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve configuration: %w", err)
		}
	}
	return &configSpec, nil
}

func resolveNodeArchitectures(ctx context.Context, client coreclientset.NodeInterface) ([]string, error) {
	ret := sets.New[string]()
	nodeList, err := client.List(ctx, metav1.ListOptions{})

	if err != nil {
		return nil, fmt.Errorf("failed to determine the node architectures: %w", err)
	}

	for _, node := range nodeList.Items {
		ret.Insert(node.Status.NodeInfo.Architecture)
	}
	return sets.List(ret), nil
}

func monitorNamespace(ctx context.Context, cancel func(), namespace string, client coreclientset.NamespaceInterface) {
reset:
	for {
		watcher, err := client.Watch(context.Background(), metav1.ListOptions{
			TypeMeta:      metav1.TypeMeta{},
			FieldSelector: fields.Set{"metadata.name": namespace}.AsSelector().String(),
			Watch:         true,
		})
		if err != nil {
			logrus.WithError(err).Warn("Could not start a watch on our test namespace.")
			cancel()
			return
		}
		for {
			select {
			case <-ctx.Done():
				// we're done operating anyway
				return
			case event, ok := <-watcher.ResultChan():
				if !ok {
					continue reset
				}
				ns, ok := event.Object.(*coreapi.Namespace)
				if !ok {
					continue
				}
				if ns.Name != namespace {
					continue
				}
				if ns.DeletionTimestamp != nil {
					logrus.Info("The namespace in which this test is executing has been deleted, cancelling the test...")
					cancel()
					return
				}
			}
		}
	}
}

func addSchemes() error {
	if err := imageapi.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add imagev1 to scheme: %w", err)
	}
	if err := routev1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add routev1 to scheme: %w", err)
	}
	if err := appsv1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add appsv1 to scheme: %w", err)
	}
	if err := buildv1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add buildv1 to scheme: %w", err)
	}
	if err := templateapi.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add templatev1 to scheme: %w", err)
	}
	if err := hivev1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add hivev1 to scheme: %w", err)
	}
	if err := egressfirewallv1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add egressfirewallv1 to scheme: %w", err)
	}
	if err := csiapi.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add secrets-store-csi-driver to scheme: %w", err)
	}
	return nil
}

// getClusterProfileSecret retrieves the cluster profile secret name using config resolver,
// and gets the secret from the ci namespace
func getClusterProfileSecret(cp clusterProfileForTarget, client ctrlruntimeclient.Client, resolverClient server.ResolverClient, ctx context.Context) (*coreapi.Secret, error) {
	// Use config-resolver to get details about the cluster profile (which includes the secret's name)
	cpDetails, err := resolverClient.ClusterProfile(cp.profileName)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve details from config resolver for '%s' cluster cp", cp.profileName)
	}
	// Get the secret from the ci namespace. We expect it exists
	ciSecret := &coreapi.Secret{}
	err = client.Get(ctx, ctrlruntimeclient.ObjectKey{Namespace: "ci", Name: cpDetails.Secret}, ciSecret)
	if err != nil {
		return nil, fmt.Errorf("failed to get secret '%s' from ci namespace: %w", cpDetails.Secret, err)
	}

	newSecret := &coreapi.Secret{
		Data: ciSecret.Data,
		Type: ciSecret.Type,
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-cluster-profile", cp.target),
		},
	}

	return newSecret, nil
}

type clusterProfileForTarget struct {
	target      string
	profileName string
}

// getClusterProfileNamesFromTargets extracts the needed cluster profile name(s) from the target arg(s)
func (o *options) getClusterProfileNamesFromTargets() {
	for _, targetName := range o.targets.values {
		for _, test := range o.configSpec.Tests {
			if targetName != test.As {
				continue
			}
			profile := test.GetClusterProfileName()
			if profile != "" {
				o.clusterProfiles = append(o.clusterProfiles, clusterProfileForTarget{
					target:      test.As,
					profileName: profile,
				})
			}
		}
	}
}
