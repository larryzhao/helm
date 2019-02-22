/*
Copyright The Helm Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"k8s.io/helm/pkg/chartutil"
	"k8s.io/helm/pkg/helm"
	"k8s.io/helm/pkg/renderutil"
)

const istioUpgradeDesc = `This command performs a gradually upgrade.`

type istioUpgradeCmd struct {
	release      string
	chart        string
	out          io.Writer
	client       helm.Interface
	dryRun       bool
	recreate     bool
	force        bool
	disableHooks bool
	valueFiles   valueFiles
	values       []string
	stringValues []string
	fileValues   []string
	verify       bool
	keyring      string
	install      bool
	namespace    string
	version      string
	timeout      int64
	resetValues  bool
	reuseValues  bool
	wait         bool
	repoURL      string
	username     string
	password     string
	devel        bool
	description  string

	certFile string
	keyFile  string
	caFile   string
}

type istioUpgradeOptions struct {
	currentVersion string
	targetVersion  string
	chartPath      string
	replicaCount   int
}

func newIstioUpgradeCmd(client helm.Interface, out io.Writer) *cobra.Command {
	upgrade := &istioUpgradeCmd{
		out:    out,
		client: client,
	}

	cmd := &cobra.Command{
		Use:     "istio-upgrade [RELEASE] [CHART]",
		Short:   "istio upgrade a release",
		Long:    upgradeDesc,
		PreRunE: func(_ *cobra.Command, _ []string) error { return setupConnection() },
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkArgsLength(len(args), "release name", "chart path"); err != nil {
				return err
			}

			if upgrade.version == "" && upgrade.devel {
				debug("setting version to >0.0.0-0")
				upgrade.version = ">0.0.0-0"
			}

			upgrade.release = args[0]
			upgrade.chart = args[1]
			upgrade.client = ensureHelmClient(upgrade.client)

			return upgrade.run()
		},
	}

	f := cmd.Flags()
	settings.AddFlagsTLS(f)
	f.VarP(&upgrade.valueFiles, "values", "f", "specify values in a YAML file or a URL(can specify multiple)")
	f.BoolVar(&upgrade.dryRun, "dry-run", false, "simulate an upgrade")
	f.BoolVar(&upgrade.recreate, "recreate-pods", false, "performs pods restart for the resource if applicable")
	f.BoolVar(&upgrade.force, "force", false, "force resource update through delete/recreate if needed")
	f.StringArrayVar(&upgrade.values, "set", []string{}, "set values on the command line (can specify multiple or separate values with commas: key1=val1,key2=val2)")
	f.StringArrayVar(&upgrade.stringValues, "set-string", []string{}, "set STRING values on the command line (can specify multiple or separate values with commas: key1=val1,key2=val2)")
	f.StringArrayVar(&upgrade.fileValues, "set-file", []string{}, "set values from respective files specified via the command line (can specify multiple or separate values with commas: key1=path1,key2=path2)")
	f.BoolVar(&upgrade.disableHooks, "disable-hooks", false, "disable pre/post upgrade hooks. DEPRECATED. Use no-hooks")
	f.BoolVar(&upgrade.disableHooks, "no-hooks", false, "disable pre/post upgrade hooks")
	f.BoolVar(&upgrade.verify, "verify", false, "verify the provenance of the chart before upgrading")
	f.StringVar(&upgrade.keyring, "keyring", defaultKeyring(), "path to the keyring that contains public signing keys")
	f.BoolVarP(&upgrade.install, "install", "i", false, "if a release by this name doesn't already exist, run an install")
	f.StringVar(&upgrade.namespace, "namespace", "", "namespace to install the release into (only used if --install is set). Defaults to the current kube config namespace")
	f.StringVar(&upgrade.version, "version", "", "specify the exact chart version to use. If this is not specified, the latest version is used")
	f.Int64Var(&upgrade.timeout, "timeout", 300, "time in seconds to wait for any individual Kubernetes operation (like Jobs for hooks)")
	f.BoolVar(&upgrade.resetValues, "reset-values", false, "when upgrading, reset the values to the ones built into the chart")
	f.BoolVar(&upgrade.reuseValues, "reuse-values", false, "when upgrading, reuse the last release's values and merge in any overrides from the command line via --set and -f. If '--reset-values' is specified, this is ignored.")
	f.BoolVar(&upgrade.wait, "wait", false, "if set, will wait until all Pods, PVCs, Services, and minimum number of Pods of a Deployment are in a ready state before marking the release as successful. It will wait for as long as --timeout")
	f.StringVar(&upgrade.repoURL, "repo", "", "chart repository url where to locate the requested chart")
	f.StringVar(&upgrade.username, "username", "", "chart repository username where to locate the requested chart")
	f.StringVar(&upgrade.password, "password", "", "chart repository password where to locate the requested chart")
	f.StringVar(&upgrade.certFile, "cert-file", "", "identify HTTPS client using this SSL certificate file")
	f.StringVar(&upgrade.keyFile, "key-file", "", "identify HTTPS client using this SSL key file")
	f.StringVar(&upgrade.caFile, "ca-file", "", "verify certificates of HTTPS-enabled servers using this CA bundle")
	f.BoolVar(&upgrade.devel, "devel", false, "use development versions, too. Equivalent to version '>0.0.0-0'. If --version is set, this is ignored.")
	f.StringVar(&upgrade.description, "description", "", "specify the description to use for the upgrade, rather than the default")

	f.MarkDeprecated("disable-hooks", "use --no-hooks instead")

	// set defaults from environment
	settings.InitTLS(f)

	return cmd
}

// swtichTraffic 切换流量
func (u *istioUpgradeCmd) switchTraffic(opts *istioUpgradeOptions, step int) error {
	targetVersionTraffic := 20 * step
	currentVersionTraffic := 100 - targetVersionTraffic

	fmt.Fprintf(u.out, "switching %d%% traffic to target version\n", targetVersionTraffic)
	vv := append(u.values, fmt.Sprintf("%s.trafficWeight=%d,%s.trafficWeight=%d", opts.currentVersion, currentVersionTraffic, opts.targetVersion, targetVersionTraffic))

	vvv, err := vals(u.valueFiles, vv, u.stringValues, u.fileValues, u.certFile, u.keyFile, u.caFile)
	if err != nil {
		return err
	}

	resp, err := u.client.UpdateRelease(
		u.release,
		opts.chartPath,
		helm.UpdateValueOverrides(vvv),
		helm.ReuseValues(true))

	if err != nil {
		return fmt.Errorf("UPGRADE FAILED: %v", prettyError(err))
	}

	if settings.Debug {
		printRelease(u.out, resp.Release)
	}
	fmt.Fprintf(u.out, "%d%% traffic has been switched to new release\n", targetVersionTraffic)
	nap(u.out, 60)

	return nil
}

// deployTargetVersion 部署 Target version
func (u *istioUpgradeCmd) deployTargetVersion(opts *istioUpgradeOptions) error {
	fmt.Fprintf(u.out, "deploy %d replica for target version\n", opts.replicaCount)

	// imageTag, ok := u.values["image.repository"]
	for i := 0; i < len(u.values); i++ {
		u.values[i] = strings.Replace(u.values[i], "image.repository", fmt.Sprintf("%s.image.repository", opts.targetVersion), -1)
		u.values[i] = strings.Replace(u.values[i], "image.tag", fmt.Sprintf("%s.image.tag", opts.targetVersion), -1)
	}

	vv := append(u.values, fmt.Sprintf("%s.replicaCount=%d", opts.targetVersion, opts.replicaCount))

	vvv, err := vals(u.valueFiles, vv, u.stringValues, u.fileValues, u.certFile, u.keyFile, u.caFile)
	if err != nil {
		return err
	}

	resp, err := u.client.UpdateRelease(
		u.release,
		opts.chartPath,
		helm.UpdateValueOverrides(vvv),
		helm.UpgradeDryRun(u.dryRun),
		helm.UpgradeRecreate(u.recreate),
		helm.UpgradeForce(u.force),
		helm.UpgradeDisableHooks(u.disableHooks),
		helm.UpgradeTimeout(u.timeout),
		helm.ResetValues(u.resetValues),
		helm.ReuseValues(u.reuseValues),
		helm.UpgradeWait(u.wait),
		helm.UpgradeDescription(u.description))

	if err != nil {
		return fmt.Errorf("UPGRADE FAILED: %v", prettyError(err))
	}

	if settings.Debug {
		printRelease(u.out, resp.Release)
	}
	fmt.Fprintf(u.out, "target version %q deployed\n", u.release)

	return nil
}

func (u *istioUpgradeCmd) wrapUp(opts *istioUpgradeOptions) error {
	fmt.Fprintf(u.out, "wrapping up\n")

	vv := append(u.values, fmt.Sprintf("%s.replicaCount=0,currentVersion=%s", opts.currentVersion, opts.targetVersion))
	vvv, err := vals(u.valueFiles, vv, u.stringValues, u.fileValues, u.certFile, u.keyFile, u.caFile)
	if err != nil {
		return err
	}

	resp, err := u.client.UpdateRelease(
		u.release,
		opts.chartPath,
		helm.UpdateValueOverrides(vvv),
		helm.ReuseValues(true))

	if err != nil {
		return fmt.Errorf("UPGRADE FAILED: %v", prettyError(err))
	}

	if settings.Debug {
		printRelease(u.out, resp.Release)
	}
	fmt.Fprintf(u.out, "done upgrading\n")

	return nil
}

func (u *istioUpgradeCmd) run() error {
	chartPath, err := locateChartPath(u.repoURL, u.username, u.password, u.chart, u.version, u.verify, u.keyring, u.certFile, u.keyFile, u.caFile)
	if err != nil {
		return err
	}

	// Check chart requirements to make sure all dependencies are present in /charts
	if ch, err := chartutil.Load(chartPath); err == nil {
		if req, err := chartutil.LoadRequirements(ch); err == nil {
			if err := renderutil.CheckDependencies(ch, req); err != nil {
				return err
			}
		} else if err != chartutil.ErrRequirementsNotFound {
			return fmt.Errorf("cannot load requirements: %v", err)
		}
	} else {
		return prettyError(err)
	}

	res, err := u.client.ReleaseContent(u.release, helm.ContentReleaseVersion(0))
	if err != nil {
		return prettyError(err)
	}

	values, err := chartutil.ReadValues([]byte(res.Release.Config.Raw))
	if err != nil {
		return err
	}

	values, err = chartutil.CoalesceValues(res.Release.Chart, res.Release.Config)
	if err != nil {
		return err
	}

	opts := istioUpgradeOptions{
		currentVersion: values["currentVersion"].(string),
		replicaCount:   int(values["replicaCount"].(float64)),
		chartPath:      chartPath,
	}

	if opts.currentVersion == "vx" {
		opts.targetVersion = "vy"
	} else {
		opts.targetVersion = "vx"
	}
	fmt.Fprintf(u.out, "start upgrade, currentVersion: %s, targetVersion: %s\n", opts.currentVersion, opts.targetVersion)

	// Start deploy
	err = u.deployTargetVersion(&opts)
	if err != nil {
		return err
	}
	fmt.Println("deployed")
	nap(u.out, 60)

	// Start traffic switching
	maxSteps := 5
	for step := 1; step <= maxSteps; step++ {
		if err := u.switchTraffic(&opts, step); err != nil {
			return err
		}
	}

	// Wrapup
	err = u.wrapUp(&opts)
	if err != nil {
		return err
	}

	// Print the status like status command does
	status, err := u.client.ReleaseStatus(u.release)
	if err != nil {
		return prettyError(err)
	}
	PrintStatus(u.out, status)

	return nil
}

func nap(out io.Writer, seconds int) {
	fmt.Fprintf(out, "napping for %ds\n", seconds)
	time.Sleep(time.Duration(seconds) * time.Second)
}
