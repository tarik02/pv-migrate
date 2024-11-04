package strategy

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/utkuozdemir/pv-migrate/k8s"
	"github.com/utkuozdemir/pv-migrate/migration"
	"github.com/utkuozdemir/pv-migrate/rsync"
	"github.com/utkuozdemir/pv-migrate/ssh"
	"github.com/utkuozdemir/pv-migrate/util"
)

type LbSvc struct{}

func (r *LbSvc) Run(ctx context.Context, attempt *migration.Attempt, logger *slog.Logger) error {
	mig := attempt.Migration

	sourceInfo := mig.SourceInfo
	destInfo := mig.DestInfo
	sourceNs := sourceInfo.Claim.Namespace
	destNs := destInfo.Claim.Namespace
	keyAlgorithm := mig.Request.KeyAlgorithm

	logger.Info("🔑 Generating SSH key pair", "algorithm", keyAlgorithm)

	publicKey, privateKey, err := ssh.CreateSSHKeyPair(keyAlgorithm)
	if err != nil {
		return fmt.Errorf("failed to create ssh key pair: %w", err)
	}

	privateKeyMountPath := "/tmp/id_" + keyAlgorithm

	srcReleaseName := attempt.HelmReleaseNamePrefix + "-src"
	destReleaseName := attempt.HelmReleaseNamePrefix + "-dest"
	releaseNames := []string{srcReleaseName, destReleaseName}

	doneCh := registerCleanupHook(attempt, releaseNames, logger)
	defer cleanupAndReleaseHook(ctx, attempt, releaseNames, doneCh, logger)

	err = installOnDest(attempt, destReleaseName, publicKey, destMountPath, logger)
	if err != nil {
		return fmt.Errorf("failed to install on source: %w", err)
	}

	destKubeClient := attempt.Migration.DestInfo.ClusterClient.KubeClient
	svcName := destReleaseName + "-sshd"

	lbSvcAddress, err := k8s.GetServiceAddress(ctx, destKubeClient, destNs, svcName, mig.Request.LBSvcTimeout)
	if err != nil {
		return fmt.Errorf("failed to get service address: %w", err)
	}

	sshTargetHost := formatSSHTargetHost(lbSvcAddress)
	if mig.Request.DestHostOverride != "" {
		sshTargetHost = mig.Request.DestHostOverride
	}

	err = installOnSource(attempt, srcReleaseName, privateKey, privateKeyMountPath,
		sshTargetHost, srcMountPath, destMountPath, logger)
	if err != nil {
		return fmt.Errorf("failed to install on dest: %w", err)
	}

	showProgressBar := !attempt.Migration.Request.NoProgressBar
	kubeClient := sourceInfo.ClusterClient.KubeClient
	jobName := srcReleaseName + "-rsync"

	if err = k8s.WaitForJobCompletion(ctx, kubeClient, sourceNs, jobName, showProgressBar, logger); err != nil {
		return fmt.Errorf("failed to wait for job completion: %w", err)
	}

	return nil
}

func installOnDest(attempt *migration.Attempt, releaseName,
	publicKey, destMountPath string, logger *slog.Logger,
) error {
	mig := attempt.Migration
	destInfo := mig.DestInfo
	namespace := destInfo.Claim.Namespace

	vals := map[string]any{
		"sshd": map[string]any{
			"enabled":   true,
			"namespace": namespace,
			"publicKey": publicKey,
			"service": map[string]any{
				"type": "LoadBalancer",
			},
			"pvcMounts": []map[string]any{
				{
					"name":      destInfo.Claim.Name,
					"mountPath": destMountPath,
				},
			},
			"affinity": destInfo.AffinityHelmValues,
		},
	}

	return installHelmChart(attempt, destInfo, releaseName, vals, logger)
}

func installOnSource(attempt *migration.Attempt, releaseName, privateKey,
	privateKeyMountPath, sshHost, srcMountPath, destMountPath string, logger *slog.Logger,
) error {
	mig := attempt.Migration
	srcInfo := mig.SourceInfo
	namespace := srcInfo.Claim.Namespace

	srcPath := srcMountPath + "/" + mig.Request.Source.Path
	destPath := destMountPath + "/" + mig.Request.Dest.Path
	rsyncCmd := rsync.Cmd{
		NoChown:     mig.Request.NoChown,
		Delete:      mig.Request.DeleteExtraneousFiles,
		SrcPath:     srcPath,
		DestPath:    destPath,
		DestUseSSH:  true,
		DestSSHHost: sshHost,
		Compress:    mig.Request.Compress,
	}

	rsyncCmdStr, err := rsyncCmd.Build()
	if err != nil {
		return fmt.Errorf("failed to build rsync command: %w", err)
	}

	vals := map[string]any{
		"rsync": map[string]any{
			"enabled":             true,
			"namespace":           namespace,
			"privateKeyMount":     true,
			"privateKey":          privateKey,
			"privateKeyMountPath": privateKeyMountPath,
			"sshRemoteHost":       sshHost,
			"pvcMounts": []map[string]any{
				{
					"name":      srcInfo.Claim.Name,
					"readOnly":  mig.Request.SourceMountReadOnly,
					"mountPath": srcMountPath,
				},
			},
			"command":  rsyncCmdStr,
			"affinity": srcInfo.AffinityHelmValues,
		},
	}

	return installHelmChart(attempt, srcInfo, releaseName, vals, logger)
}

func formatSSHTargetHost(host string) string {
	if util.IsIPv6(host) {
		return fmt.Sprintf("[%s]", host)
	}

	return host
}
