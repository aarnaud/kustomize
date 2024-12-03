// Copyright 2019 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

// Helm chart inflation generator.
// Uses helm V3 to generate k8s YAML from a helm chart.

//go:generate pluginator
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/errors"
	"sigs.k8s.io/kustomize/kyaml/kio"
	kyaml "sigs.k8s.io/kustomize/kyaml/yaml"
	"sigs.k8s.io/kustomize/kyaml/yaml/merge2"
	"sigs.k8s.io/yaml"
)

// Generate resources from a remote or local helm chart.
type plugin struct {
	h *resmap.PluginHelpers
	types.HelmGlobals
	types.HelmChart
	tmpDir string
}

var KustomizePlugin plugin //nolint:gochecknoglobals

const (
	valuesMergeOptionMerge    = "merge"
	valuesMergeOptionOverride = "override"
	valuesMergeOptionReplace  = "replace"
)

var legalMergeOptions = []string{
	valuesMergeOptionMerge,
	valuesMergeOptionOverride,
	valuesMergeOptionReplace,
}

// Config uses the input plugin configurations `config` to setup the generator
// options
func (p *plugin) Config(
	h *resmap.PluginHelpers, config []byte) (err error) {
	if h.GeneralConfig() == nil {
		return fmt.Errorf("unable to access general config")
	}
	if !h.GeneralConfig().HelmConfig.Enabled {
		return fmt.Errorf("must specify --enable-helm")
	}
	if h.GeneralConfig().HelmConfig.Command == "" {
		return fmt.Errorf("must specify --helm-command")
	}

	// CLI args takes precedence
	if h.GeneralConfig().HelmConfig.KubeVersion != "" {
		p.HelmChart.KubeVersion = h.GeneralConfig().HelmConfig.KubeVersion
	}
	if len(h.GeneralConfig().HelmConfig.ApiVersions) != 0 {
		p.HelmChart.ApiVersions = h.GeneralConfig().HelmConfig.ApiVersions
	}
	if h.GeneralConfig().HelmConfig.Debug {
		p.HelmChart.Debug = h.GeneralConfig().HelmConfig.Debug
	}

	p.h = h
	if err = yaml.Unmarshal(config, p); err != nil {
		return
	}
	return p.validateArgs()
}

// This uses the real file system since tmpDir may be used
// by the helm subprocess.  Cannot use a chroot jail or fake
// filesystem since we allow the user to use previously
// downloaded charts.  This is safe since this plugin is
// owned by kustomize.
func (p *plugin) establishTmpDir() (err error) {
	if p.tmpDir != "" {
		// already done.
		return nil
	}
	p.tmpDir, err = os.MkdirTemp("", "kustomize-helm-")
	return err
}

func (p *plugin) validateArgs() (err error) {
	if p.Name == "" {
		return fmt.Errorf("chart name cannot be empty")
	}

	// ChartHome might be consulted by the plugin (to read
	// values files below it), so it must be located under
	// the loader root (unless root restrictions are
	// disabled, in which case this can be an absolute path).
	if p.ChartHome == "" {
		p.ChartHome = types.HelmDefaultHome
	}

	// The ValuesFile(s) may be consulted by the plugin, so it must
	// be under the loader root (unless root restrictions are
	// disabled).
	if p.ValuesFile == "" {
		p.ValuesFile = filepath.Join(p.absChartHome(), p.Name, "values.yaml")
	}
	for i, file := range p.AdditionalValuesFiles {
		// use Load() to enforce root restrictions
		if _, err := p.h.Loader().Load(file); err != nil {
			return errors.WrapPrefixf(err, "could not load additionalValuesFile")
		}
		// the additional values filepaths must be relative to the kust root
		p.AdditionalValuesFiles[i] = filepath.Join(p.h.Loader().Root(), file)
	}

	if err = p.errIfIllegalValuesMerge(); err != nil {
		return err
	}

	// ConfigHome is not loaded by the plugin, and can be located anywhere.
	if p.ConfigHome == "" {
		if err = p.establishTmpDir(); err != nil {
			return errors.WrapPrefixf(
				err, "unable to create tmp dir for HELM_CONFIG_HOME")
		}
		p.ConfigHome = filepath.Join(p.tmpDir, "helm")
	}
	return nil
}

func (p *plugin) errIfIllegalValuesMerge() error {
	if p.ValuesMerge == "" {
		// Use the default.
		p.ValuesMerge = valuesMergeOptionOverride
		return nil
	}
	for _, opt := range legalMergeOptions {
		if p.ValuesMerge == opt {
			return nil
		}
	}
	return fmt.Errorf("valuesMerge must be one of %v", legalMergeOptions)
}

func (p *plugin) absChartHome() string {
	var chartHome string
	if filepath.IsAbs(p.ChartHome) {
		chartHome = p.ChartHome
	} else {
		chartHome = filepath.Join(p.h.Loader().Root(), p.ChartHome)
	}

	if p.Version != "" {
		return filepath.Join(chartHome, fmt.Sprintf("%s-%s", p.Name, p.Version))
	}
	return chartHome
}

func (p *plugin) runHelmCommand(
	args []string) ([]byte, error) {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	cmd := exec.Command(p.h.GeneralConfig().HelmConfig.Command, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	env := []string{
		fmt.Sprintf("HELM_CONFIG_HOME=%s", p.ConfigHome),
		fmt.Sprintf("HELM_CACHE_HOME=%s/.cache", p.ConfigHome),
		fmt.Sprintf("HELM_DATA_HOME=%s/.data", p.ConfigHome)}
	cmd.Env = append(os.Environ(), env...)
	err := cmd.Run()
	errorOutput := stderr.String()
	if slices.Contains(args, "--debug") {
		errorOutput = " Helm stack trace:\n" + errorOutput + "\nHelm template:\n" + stdout.String() + "\n"
	}
	if err != nil {
		helm := p.h.GeneralConfig().HelmConfig.Command
		err = errors.WrapPrefixf(
			fmt.Errorf(
				"unable to run: '%s %s' with env=%s (is '%s' installed?): %w",
				helm, strings.Join(args, " "), env, helm, err),
			errorOutput,
		)
	}
	return stdout.Bytes(), err
}

// createNewMergedValuesFile replaces/merges original values file with ValuesInline.
func (p *plugin) createNewMergedValuesFile() (
	path string, err error) {
	if p.ValuesMerge == valuesMergeOptionMerge ||
		p.ValuesMerge == valuesMergeOptionOverride {
		if err = p.replaceValuesInline(); err != nil {
			return "", err
		}
	}
	var b []byte
	b, err = yaml.Marshal(p.ValuesInline)
	if err != nil {
		return "", err
	}
	return p.writeValuesBytes(b)
}

func (p *plugin) replaceValuesInline() error {
	pValues, err := p.h.Loader().Load(p.ValuesFile)
	if err != nil {
		return err
	}
	chValues, err := kyaml.Parse(string(pValues))
	if err != nil {
		return errors.WrapPrefixf(err, "could not parse values file into rnode")
	}
	inlineValues, err := kyaml.FromMap(p.ValuesInline)
	if err != nil {
		return errors.WrapPrefixf(err, "could not parse values inline into rnode")
	}
	var outValues *kyaml.RNode
	switch p.ValuesMerge {
	// Function `merge2.Merge` overrides values in dest with values from src.
	// To achieve override or merge behavior, we pass parameters in different order.
	// Object passed as dest will be modified, so we copy it just in case someone
	// decides to use it after this is called.
	case valuesMergeOptionOverride:
		outValues, err = merge2.Merge(inlineValues, chValues.Copy(), kyaml.MergeOptions{})
	case valuesMergeOptionMerge:
		outValues, err = merge2.Merge(chValues, inlineValues.Copy(), kyaml.MergeOptions{})
	}
	if err != nil {
		return errors.WrapPrefixf(err, "could not merge values")
	}
	mapValues, err := outValues.Map()
	if err != nil {
		return errors.WrapPrefixf(err, "could not parse merged values into map")
	}
	p.ValuesInline = mapValues
	return err
}

// copyValuesFile to avoid branching.  TODO: get rid of this.
func (p *plugin) copyValuesFile() (string, error) {
	b, err := p.h.Loader().Load(p.ValuesFile)
	if err != nil {
		return "", err
	}
	return p.writeValuesBytes(b)
}

// Write a absolute path file in the tmp file system.
func (p *plugin) writeValuesBytes(
	b []byte) (string, error) {
	if err := p.establishTmpDir(); err != nil {
		return "", fmt.Errorf("cannot create tmp dir to write helm values")
	}
	path := filepath.Join(p.tmpDir, p.Name+"-kustomize-values.yaml")
	return path, errors.WrapPrefixf(os.WriteFile(path, b, 0644), "failed to write values file")
}

func (p *plugin) cleanup() {
	if p.tmpDir != "" {
		os.RemoveAll(p.tmpDir)
	}
}

// Generate implements generator
func (p *plugin) Generate() (rm resmap.ResMap, err error) {
	defer p.cleanup()
	if err = p.checkHelmVersion(); err != nil {
		return nil, err
	}
	if path, exists := p.chartExistsLocally(); !exists {
		if p.Repo == "" {
			return nil, fmt.Errorf(
				"no repo specified for pull, no chart found at '%s'", path)
		}
		if _, err := p.runHelmCommand(p.pullCommand()); err != nil {
			return nil, err
		}
	}
	if len(p.ValuesInline) > 0 {
		p.ValuesFile, err = p.createNewMergedValuesFile()
	} else {
		p.ValuesFile, err = p.copyValuesFile()
	}
	if err != nil {
		return nil, err
	}
	var stdout []byte
	stdout, err = p.runHelmCommand(p.AsHelmArgs(p.absChartHome()))
	if err != nil {
		return nil, err
	}

	rm, resMapErr := p.h.ResmapFactory().NewResMapFromBytes(stdout)
	if resMapErr == nil {
		return rm, nil
	}
	// try to remove the contents before first "---" because
	// helm may produce messages to stdout before it
	r := &kio.ByteReader{Reader: bytes.NewBufferString(string(stdout)), OmitReaderAnnotations: true}
	nodes, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("error reading helm output: %w", err)
	}

	if len(nodes) != 0 {
		rm, err = p.h.ResmapFactory().NewResMapFromRNodeSlice(nodes)
		if err != nil {
			return nil, fmt.Errorf("could not parse rnode slice into resource map: %w", err)
		}
		return rm, nil
	}
	return nil, fmt.Errorf("could not parse bytes into resource map: %w", resMapErr)
}

func (p *plugin) pullCommand() []string {
	args := []string{
		"pull",
		"--untar",
		"--untardir", p.absChartHome(),
	}

	switch {
	case strings.HasPrefix(p.Repo, "oci://"):
		args = append(args, strings.TrimSuffix(p.Repo, "/")+"/"+p.Name)
	case p.Repo != "":
		args = append(args, "--repo", p.Repo)
		fallthrough
	default:
		args = append(args, p.Name)
	}

	if p.Version != "" {
		args = append(args, "--version", p.Version)
	}
	return args
}

// chartExistsLocally will return true if the chart does exist in
// local chart home.
func (p *plugin) chartExistsLocally() (string, bool) {
	path := filepath.Join(p.absChartHome(), p.Name)
	s, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	return path, s.IsDir()
}

// checkHelmVersion will return an error if the helm version is not V3
func (p *plugin) checkHelmVersion() error {
	stdout, err := p.runHelmCommand([]string{"version", "-c", "--short"})
	if err != nil {
		return err
	}
	r, err := regexp.Compile(`v?\d+(\.\d+)+`)
	if err != nil {
		return err
	}
	v := r.FindString(string(stdout))
	if v == "" {
		return fmt.Errorf("cannot find version string in %s", string(stdout))
	}
	if v[0] == 'v' {
		v = v[1:]
	}
	majorVersion := strings.Split(v, ".")[0]
	if majorVersion != "3" {
		return fmt.Errorf("this plugin requires helm V3 but got v%s", v)
	}
	return nil
}
