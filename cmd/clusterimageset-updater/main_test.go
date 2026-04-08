package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/client-go/kubernetes/scheme"

	hivev1 "github.com/openshift/hive/apis/hive/v1"

	"github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/testhelper"
)

func TestArchitectureForBounds(t *testing.T) {
	tests := []struct {
		name    string
		bounds  api.VersionBounds
		want    api.ReleaseArchitecture
		wantErr bool
	}{
		{"4.7 uses amd64", api.VersionBounds{Lower: "4.7.0-0", Upper: "4.8.0-0"}, api.ReleaseArchitectureAMD64, false},
		{"4.11 uses amd64", api.VersionBounds{Lower: "4.11.0-0", Upper: "4.12.0-0"}, api.ReleaseArchitectureAMD64, false},
		{"4.12 uses multi", api.VersionBounds{Lower: "4.12.0-0", Upper: "4.13.0-0"}, api.ReleaseArchitectureMULTI, false},
		{"4.13 uses multi", api.VersionBounds{Lower: "4.13.0-0", Upper: "4.14.0-0"}, api.ReleaseArchitectureMULTI, false},
		{"4.21 uses multi", api.VersionBounds{Lower: "4.21.0-0", Upper: "4.22.0-0"}, api.ReleaseArchitectureMULTI, false},
		{"5.0 uses multi", api.VersionBounds{Lower: "5.0.0-0", Upper: "5.1.0-0"}, api.ReleaseArchitectureMULTI, false},
		{"5.1 uses multi", api.VersionBounds{Lower: "5.1.0-0", Upper: "5.2.0-0"}, api.ReleaseArchitectureMULTI, false},
		{"v4.12 uses multi", api.VersionBounds{Lower: "v4.12.0-0", Upper: "4.13.0-0"}, api.ReleaseArchitectureMULTI, false},
		{"major 3 uses amd64", api.VersionBounds{Lower: "3.12.0-0", Upper: "4.0.0-0"}, api.ReleaseArchitectureAMD64, false},
		{"unparseable lower fails", api.VersionBounds{Lower: "bad", Upper: "4.8.0-0"}, "", true},
		{"single segment fails", api.VersionBounds{Lower: "4", Upper: "4.8.0-0"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := architectureForBounds(tt.bounds)
			if (err != nil) != tt.wantErr {
				t.Errorf("architectureForBounds() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("architectureForBounds() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldDeleteClusterImageSet(t *testing.T) {
	bounds := api.VersionBounds{Lower: "4.18.0-0", Upper: "4.19.0-0", Stream: "4-stable"}
	poolFilesByBounds := map[api.VersionBounds][]string{
		bounds: {"pool.yaml"},
	}
	boundsToPullspec := map[api.VersionBounds]string{
		bounds: "quay.io/openshift-release-dev/ocp-release:4.18.1-multi",
	}

	tests := []struct {
		name          string
		imageset      hivev1.ClusterImageSet
		usedImageSets map[string]struct{}
		wantDelete    bool
		wantErr       string
	}{
		{
			name: "do not delete imageset referenced by any pool",
			imageset: hivev1.ClusterImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: "still-in-use",
					Annotations: map[string]string{
						versionLowerLabel:  "4.17.0-0",
						versionUpperLabel:  "4.18.0-0",
						versionStreamLabel: "4-stable",
					},
				},
				Spec: hivev1.ClusterImageSetSpec{ReleaseImage: "old"},
			},
			usedImageSets: map[string]struct{}{"still-in-use": {}},
			wantDelete:    false,
		},
		{
			name: "delete imageset when bounds are no longer referenced by pools",
			imageset: hivev1.ClusterImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: "unused",
					Annotations: map[string]string{
						versionLowerLabel:  "4.17.0-0",
						versionUpperLabel:  "4.18.0-0",
						versionStreamLabel: "4-stable",
					},
				},
				Spec: hivev1.ClusterImageSetSpec{ReleaseImage: "old"},
			},
			usedImageSets: map[string]struct{}{},
			wantDelete:    true,
		},
		{
			name: "keep imageset when bounds and pullspec match",
			imageset: hivev1.ClusterImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: "current",
					Annotations: map[string]string{
						versionLowerLabel:  bounds.Lower,
						versionUpperLabel:  bounds.Upper,
						versionStreamLabel: bounds.Stream,
					},
				},
				Spec: hivev1.ClusterImageSetSpec{ReleaseImage: boundsToPullspec[bounds]},
			},
			usedImageSets: map[string]struct{}{},
			wantDelete:    false,
		},
		{
			name: "delete imageset when pullspec drifts from current bounds",
			imageset: hivev1.ClusterImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: "drifted",
					Annotations: map[string]string{
						versionLowerLabel:  bounds.Lower,
						versionUpperLabel:  bounds.Upper,
						versionStreamLabel: bounds.Stream,
					},
				},
				Spec: hivev1.ClusterImageSetSpec{ReleaseImage: "quay.io/example/old:tag"},
			},
			usedImageSets: map[string]struct{}{},
			wantDelete:    true,
		},
		{
			name: "do not delete imageset without version annotations",
			imageset: hivev1.ClusterImageSet{
				ObjectMeta: metav1.ObjectMeta{Name: "manual-imageset"},
				Spec:       hivev1.ClusterImageSetSpec{ReleaseImage: "whatever"},
			},
			usedImageSets: map[string]struct{}{},
			wantDelete:    false,
		},
		{
			name: "error on malformed version annotations",
			imageset: hivev1.ClusterImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: "malformed",
					Annotations: map[string]string{
						versionUpperLabel: "4.19.0-0",
					},
				},
			},
			usedImageSets: map[string]struct{}{},
			wantErr:       "Failed to parse version labels for clusterimageset malformed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDelete, err := shouldDeleteClusterImageSet(tt.imageset, tt.usedImageSets, poolFilesByBounds, boundsToPullspec)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q but got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain expected substring %q", err.Error(), tt.wantErr)
				}
				return
			}
			if gotDelete != tt.wantDelete {
				t.Fatalf("shouldDeleteClusterImageSet() = %v, want %v", gotDelete, tt.wantDelete)
			}
		})
	}
}

func TestEnsureLabels(t *testing.T) {
	testCases := []struct {
		name             string
		given            hivev1.ClusterPool
		expected         hivev1.ClusterPool
		expectedModified bool
	}{
		{
			name: "basic case",
			given: hivev1.ClusterPool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"owner": "dpp",
					},
				},
			},
			expected: hivev1.ClusterPool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"owner": "dpp",
					},
				},
				Spec: hivev1.ClusterPoolSpec{
					Labels: map[string]string{"tp.openshift.io/owner": "dpp"},
				},
			},
			expectedModified: true,
		},
		{
			name: "not modified",
			given: hivev1.ClusterPool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"owner": "dpp",
					},
				},
				Spec: hivev1.ClusterPoolSpec{
					Labels: map[string]string{"tp.openshift.io/owner": "dpp"},
				},
			},
			expected: hivev1.ClusterPool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"owner": "dpp",
					},
				},
				Spec: hivev1.ClusterPoolSpec{
					Labels: map[string]string{"tp.openshift.io/owner": "dpp"},
				},
			},
		},
		{
			name: "modified",
			given: hivev1.ClusterPool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"owner": "dpp",
					},
				},
				Spec: hivev1.ClusterPoolSpec{
					Labels: map[string]string{"tp.openshift.io/owner": "not-dpp"},
				},
			},
			expected: hivev1.ClusterPool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"owner": "dpp",
					},
				},
				Spec: hivev1.ClusterPoolSpec{
					Labels: map[string]string{"tp.openshift.io/owner": "dpp"},
				},
			},
			expectedModified: true,
		},
		{
			name: "given has no labels",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual, actualModified := ensureLabels(tc.given)
			if diff := cmp.Diff(tc.expected, actual); diff != "" {
				t.Errorf("%s differs from expected:\n%s", tc.name, diff)
			}
			if diff := cmp.Diff(tc.expectedModified, actualModified); diff != "" {
				t.Errorf("%s differs from expected:\n%s", tc.name, diff)
			}
		})
	}
}

func TestEnsureLabelsOnClusterPools(t *testing.T) {
	dir, err := os.MkdirTemp("", "TestEnsureLabelsOnClusterPools")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	testCases := []struct {
		name            string
		input           string
		output          string
		expected        error
		expectedContent string
	}{
		{
			name:   "basic case",
			input:  filepath.Join("testdata", "pools", "cvp-ocp-4-9-amd64-aws-us-west-2_clusterpool.yaml"),
			output: filepath.Join(dir, "cvp-ocp-4-9-amd64-aws-us-west-2_clusterpool.yaml"),
			expectedContent: `apiVersion: hive.openshift.io/v1
kind: ClusterPool
metadata:
  creationTimestamp: null
  labels:
    architecture: amd64
    cloud: aws
    owner: cvp
    product: ocp
    region: us-west-2
    version: "4.9"
    version_lower: 4.9.0-0
    version_upper: 4.10.0-0
  name: cvp-ocp-4-9-amd64-aws-us-west-2
  namespace: cvp-cluster-pool
spec:
  baseDomain: cpaas-ci.devcluster.openshift.com
  hibernationConfig:
    resumeTimeout: 15m0s
  imageSetRef:
    name: ocp-release-4.9.57-x86-64-for-4.9.0-0-to-4.10.0-0
  installAttemptsLimit: 1
  installConfigSecretTemplateRef:
    name: install-config-aws-us-west-2
  labels:
    tp.openshift.io/owner: cvp
  maxSize: 10
  platform:
    aws:
      credentialsSecretRef:
        name: cvp-aws-credentials
      region: us-west-2
  pullSecretRef:
    name: pull-secret
  runningCount: 1
  size: 5
  skipMachinePools: true
status:
  ready: 0
  size: 0
  standby: 0
`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := addSchemes(); err != nil {
				t.Fatal("Failed to set up scheme")
			}
			s := json.NewSerializerWithOptions(json.DefaultMetaFactory, scheme.Scheme,
				scheme.Scheme, json.SerializerOptions{Yaml: true, Pretty: false, Strict: false})
			actual := ensureLabelsOnClusterPool(s, tc.input, tc.output)
			if diff := cmp.Diff(tc.expected, actual, testhelper.EquateErrorMessage); diff != "" {
				t.Errorf("%s differs from expected:\n%s", tc.name, diff)
			}
			if actual == nil {
				raw, err := os.ReadFile(tc.output)
				if err != nil {
					t.Errorf("failed to read file: %v", err)
				}
				if diff := cmp.Diff(tc.expectedContent, string(raw)); diff != "" {
					t.Errorf("%s differs from expected:\n%s", tc.name, diff)
				}
			}
		})
	}
}
