package boot

import (
	"fmt"
	"github.com/jenkins-x/jx/pkg/boot"
	"github.com/jenkins-x/jx/pkg/cmd/helper"
	"github.com/jenkins-x/jx/pkg/cmd/opts"
	"github.com/jenkins-x/jx/pkg/cmd/templates"
	"github.com/jenkins-x/jx/pkg/config"
	"github.com/jenkins-x/jx/pkg/gits"
	"github.com/jenkins-x/jx/pkg/kube"
	"github.com/jenkins-x/jx/pkg/log"
	"github.com/jenkins-x/jx/pkg/util"
	"github.com/pkg/errors"
	uuid "github.com/satori/go.uuid"
	"github.com/spf13/cobra"
	"io/ioutil"
	"os"
	"strings"
)

// BootUpgradeOptions options for the command
type BootUpgradeOptions struct {
	*opts.CommonOptions
	Dir string
}

var (
	bootUpgradeLong = templates.LongDesc(`
		This command creates a pr for upgrading a jx boot gitOps cluster, incorporating changes to boot
        config and version stream ref
`)

	bootUpgradeExample = templates.Examples(`
		# create pr for upgrading a jx boot gitOps cluster
		jx boot upgrade
`)
)

// NewCmdBootUpgrade creates the command
func NewCmdBootUpgrade(commonOpts *opts.CommonOptions) *cobra.Command {
	options := &BootUpgradeOptions{
		CommonOptions: commonOpts,
	}
	cmd := &cobra.Command{
		Use:     "boot upgrade",
		Aliases: []string{"bu"},
		Short:   "Upgrades jx boot config",
		Long:    bootUpgradeLong,
		Example: bootUpgradeExample,
		Run: func(cmd *cobra.Command, args []string) {
			options.Cmd = cmd
			options.Args = args
			err := options.Run()
			helper.CheckErr(err)
		},
	}
	cmd.Flags().StringVarP(&options.Dir, "dir", "d", "", "the directory to look for the Jenkins X Pipeline and requirements")
	return cmd
}

// Run runs this command
func (o *BootUpgradeOptions) Run() error {
	if o.Dir == "" {
		err := o.cloneDevEnv()
		if err != nil {
			return errors.Wrap(err, "failed to clone dev environment repo")
		}
	}

	reqsVersionStream, err := o.requirementsVersionStream()
	if err != nil {
		return errors.Wrap(err, "failed to get requirements version stream")
	}

	upgradeVersionSha, err := o.upgradeAvailable(reqsVersionStream.URL, reqsVersionStream.Ref, "master")
	if err != nil {
		return errors.Wrap(err, "failed to get check for available update")
	}
	if upgradeVersionSha == "" {
		return nil
	}

	localBranch, err := o.checkoutNewBranch()
	if err != nil {
		return errors.Wrap(err, "failed to checkout upgrade_branch")
	}

	bootConfigURL := determineBootConfigURL(reqsVersionStream.URL)
	err = o.updateBootConfig(reqsVersionStream.URL, reqsVersionStream.Ref, bootConfigURL, upgradeVersionSha)
	if err != nil {
		return errors.Wrap(err, "failed to update boot configuration")
	}

	err = o.updateVersionStreamRef(upgradeVersionSha)
	if err != nil {
		return errors.Wrap(err, "failed to update version stream ref")
	}

	err = o.raisePR()
	if err != nil {
		return errors.Wrap(err, "failed to raise pr")
	}

	err = o.deleteLocalBranch(localBranch)
	if err != nil {
		return errors.Wrapf(err, "failed to delete local branch %s", localBranch)
	}
	return nil
}

func determineBootConfigURL(versionStreamURL string) string {
	bootConfigURL := config.DefaultBootRepository
	if versionStreamURL == config.DefaultCloudBeesVersionsURL {
		bootConfigURL = config.DefaultCloudBeesBootRepository
	}
	return bootConfigURL
}

func (o *BootUpgradeOptions) requirementsVersionStream() (*config.VersionStreamConfig, error) {
	requirements, requirementsFile, err := config.LoadRequirementsConfig(o.Dir)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to load requirements config %s", requirementsFile)
	}
	exists, err := util.FileExists(requirementsFile)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to check if file %s exists", requirementsFile)
	}
	if !exists {
		return nil, fmt.Errorf("no requirements file %s ensure you are running this command inside a GitOps clone", requirementsFile)
	}
	reqsVersionSteam := requirements.VersionStream
	return &reqsVersionSteam, nil
}

func (o *BootUpgradeOptions) upgradeAvailable(versionStreamURL string, versionStreamRef string, upgradeRef string) (string, error) {
	versionsDir, err := o.CloneJXVersionsRepo(versionStreamURL, upgradeRef)
	if err != nil {
		return "", errors.Wrapf(err, "failed to clone versions repo %s", versionStreamURL)
	}
	upgradeVersionSha, err := o.Git().GetCommitPointedToByTag(versionsDir, upgradeRef)
	if err != nil {
		return "", errors.Wrapf(err, "failed to get commit pointed to by %s", upgradeRef)
	}

	if versionStreamRef == upgradeVersionSha {
		log.Logger().Infof("No upgrade available")
		return "", nil
	}
	log.Logger().Infof("upgrade available!!!!")
	return upgradeVersionSha, nil
}

func (o *BootUpgradeOptions) checkoutNewBranch() (string, error) {
	localBranchUUID, err := uuid.NewV4()
	if err != nil {
		return "", errors.Wrapf(err, "creating UUID for local branch")
	}
	localBranch := localBranchUUID.String()
	err = o.Git().CreateBranch(o.Dir, localBranch)
	if err != nil {
		return "", errors.Wrapf(err, "failed to create local branch %s", localBranch)
	}
	err = o.Git().Checkout(o.Dir, localBranch)
	if err != nil {
		return "", errors.Wrapf(err, "failed to checkout local branch %s", localBranch)
	}
	return localBranch, nil
}

func (o *BootUpgradeOptions) updateVersionStreamRef(upgradeRef string) error {
	requirements, requirementsFile, err := config.LoadRequirementsConfig(o.Dir)
	if err != nil {
		return errors.Wrapf(err, "failed to load requirements file %s", requirementsFile)
	}

	if requirements.VersionStream.Ref != upgradeRef {
		log.Logger().Infof("Upgrading version stream ref to %s", upgradeRef)
		requirements.VersionStream.Ref = upgradeRef
		err = requirements.SaveConfig(requirementsFile)
		if err != nil {
			return errors.Wrapf(err, "failed to write version stream to %s", requirementsFile)
		}
		err = o.Git().AddCommitFiles(o.Dir, "feat: upgrade version stream", []string{requirementsFile})
		if err != nil {
			return errors.Wrapf(err, "failed to commit requirements file %s", requirementsFile)
		}
	}
	return nil
}

func (o *BootUpgradeOptions) updateBootConfig(versionStreamURL string, versionStreamRef string, bootConfigURL string, upgradeVersionSha string) error {
	configCloneDir, err := o.cloneBootConfig(bootConfigURL)
	if err != nil {
		return errors.Wrapf(err, "failed to clone boot config repo %s", bootConfigURL)
	}
	defer func() {
		err := os.RemoveAll(configCloneDir)
		if err != nil {
			log.Logger().Infof("Error removing tmpDir: %v", err)
		}
	}()

	currentSha, currentVersion, err := o.bootConfigRef(configCloneDir, versionStreamURL, versionStreamRef, bootConfigURL)
	if err != nil {
		return errors.Wrapf(err, "failed to get boot config ref for version stream: %s", versionStreamRef)
	}
	upgradeSha, upgradeVersion, err := o.bootConfigRef(configCloneDir, versionStreamURL, upgradeVersionSha, bootConfigURL)
	if err != nil {
		return errors.Wrapf(err, "failed to get boot config ref for version stream ref: %s", upgradeVersionSha)
	}

	// check if boot config upgrade available
	if upgradeSha == currentSha {
		log.Logger().Infof("No boot config upgrade available")
		return nil
	}
	log.Logger().Infof("boot config upgrade available!!!!")
	log.Logger().Infof("Upgrading from v%s to v%s", currentVersion, upgradeVersion)

	err = o.cherryPickCommits(configCloneDir, currentSha, upgradeSha)
	if err != nil {
		return errors.Wrap(err, "failed to cherry pick upgrade commits")
	}
	err = o.excludeFiles(currentSha)
	if err != nil {
		return errors.Wrap(err, "failed to exclude files from commit")
	}
	return nil
}

func (o *BootUpgradeOptions) bootConfigRef(dir string, versionStreamURL string, versionStreamRef string, configURL string) (string, string, error) {
	resolver, err := o.CreateVersionResolver(versionStreamURL, versionStreamRef)
	if err != nil {
		return "", "", errors.Wrapf(err, "failed to create version resolver %s", configURL)
	}
	configVersion, err := resolver.ResolveGitVersion(configURL)
	if err != nil {
		return "", "", errors.Wrapf(err, "failed to resolve config url %s", configURL)
	}
	cmtSha, err := o.Git().GetCommitPointedToByTag(dir, fmt.Sprintf("v%s", configVersion))
	if err != nil {
		return "", "", errors.Wrapf(err, "failed to get commit pointed to by %s", cmtSha)
	}
	return cmtSha, configVersion, nil
}

func (o *BootUpgradeOptions) cloneBootConfig(configURL string) (string, error) {
	cloneDir, err := ioutil.TempDir("", "")
	err = os.MkdirAll(cloneDir, util.DefaultWritePermissions)
	if err != nil {
		return "", errors.Wrapf(err, "failed to create directory: %s", cloneDir)
	}

	err = o.Git().CloneBare(cloneDir, configURL)
	if err != nil {
		return "", errors.Wrapf(err, "failed to clone git URL %s to directory %s", configURL, cloneDir)
	}
	return cloneDir, nil
}

func (o *BootUpgradeOptions) cherryPickCommits(cloneDir, fromSha, toSha string) error {
	cmts := make([]gits.GitCommit, 0)
	cmts, err := o.Git().GetCommits(cloneDir, fromSha, toSha)
	if err != nil {
		return errors.Wrapf(err, "failed to get commits from %s", cloneDir)
	}

	log.Logger().Infof("cherry picking commits in the range %s..%s", fromSha, toSha)
	for i := len(cmts) - 1; i >= 0; i-- {
		commitSha := cmts[i].SHA
		commitMsg := cmts[i].Subject()

		err := o.Git().CherryPickTheirs(o.Dir, commitSha)
		if err != nil {
			msg := fmt.Sprintf("commit %s is a merge but no -m option was given.", commitSha)
			if !strings.Contains(err.Error(), msg) {
				return errors.Wrapf(err, "cherry-picking %s", commitSha)
			}
		} else {
			log.Logger().Infof("%s - %s", commitSha, commitMsg)
		}
	}
	return nil
}

func (o *BootUpgradeOptions) excludeFiles(commit string) error {
	excludedFiles := []string{"OWNERS"}
	err := o.Git().CheckoutCommitFiles(o.Dir, commit, excludedFiles)
	if err != nil {
		return errors.Wrap(err, "failed to checkout files")
	}
	err = o.Git().AddCommitFiles(o.Dir, "chore: exclude files from upgrade", excludedFiles)
	if err != nil && !strings.Contains(err.Error(), "nothing to commit") {
		return errors.Wrapf(err, "failed to commit excluded files %v", excludedFiles)
	}
	return nil
}

func (o *BootUpgradeOptions) raisePR() error {
	gitInfo, err := o.Git().Info(o.Dir)
	if err != nil {
		return errors.Wrap(err, "failed to get git info")
	}

	provider, err := o.gitProvider(gitInfo)
	if err != nil {
		return errors.Wrap(err, "failed to get git provider")
	}

	upstreamInfo, err := provider.GetRepository(gitInfo.Organisation, gitInfo.Name)
	if err != nil {
		return errors.Wrapf(err, "getting repository %s/%s", gitInfo.Organisation, gitInfo.Name)
	}

	details := gits.PullRequestDetails{
		BranchName: fmt.Sprintf("jx_boot_upgrade_branch"),
		Title:      "feat(config): upgrade configuration",
		Message:    "Upgrade configuration",
	}

	filter := gits.PullRequestFilter{
		Labels: []string{
			boot.PullRequestLabel,
		},
	}
	_, err = gits.PushRepoAndCreatePullRequest(o.Dir, upstreamInfo, nil, "master", &details, &filter, false, details.Title, true, false, o.Git(), provider, []string{boot.PullRequestLabel})
	if err != nil {
		return errors.Wrapf(err, "failed to create PR for base %s and head branch %s", "master", details.BranchName)
	}
	return nil
}

func (o *BootUpgradeOptions) deleteLocalBranch(branch string) error {
	err := o.Git().Checkout(o.Dir, "master")
	if err != nil {
		return errors.Wrapf(err, "failed to checkout master branch")
	}
	err = o.Git().DeleteLocalBranch(o.Dir, branch)
	if err != nil {
		return errors.Wrapf(err, "failed to delete local branch %s", branch)
	}
	return nil
}

func (o *BootUpgradeOptions) cloneDevEnv() error {
	jxClient, devNs, err := o.JXClientAndDevNamespace()
	devEnv, err := kube.GetDevEnvironment(jxClient, devNs)
	if err != nil {
		return errors.Wrapf(err, "failed to get dev environment in namespace %s", devNs)
	}
	devEnvURL := devEnv.Spec.Source.URL

	cloneDir, err := ioutil.TempDir("", "")
	err = os.MkdirAll(cloneDir, util.DefaultWritePermissions)
	if err != nil {
		return errors.Wrapf(err, "failed to create directory for dev env clone: %s", cloneDir)
	}
	err = o.Git().Clone(devEnvURL, cloneDir)
	if err != nil {
		return errors.Wrapf(err, "failed to clone git URL %s to directory %s", devEnvURL, cloneDir)
	}

	o.Dir = cloneDir
	return nil
}

func (o *BootUpgradeOptions) gitProvider(gitInfo *gits.GitRepository) (gits.GitProvider, error) {
	authConfigSvc, err := o.CreatePipelineUserGitAuthConfigService()
	if err != nil {
		return nil, errors.Wrap(err, "failed to create pipeline user git auth config service")
	}
	server, userAuth := authConfigSvc.Config().GetPipelineAuth()

	gitKind, err := o.GitServerKind(gitInfo)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get git kind")
	}

	log.Logger().Infof("gitKind %s", gitKind)

	provider, err := gitInfo.CreateProviderForUser(server, userAuth, gitKind, o.Git())
	if err != nil {
		return nil, errors.Wrap(err, "failed to create provider for user")
	}
	return provider, nil
}