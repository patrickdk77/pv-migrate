package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"maps"

	"github.com/utkuozdemir/pv-migrate/internal/migration"
	"github.com/utkuozdemir/pv-migrate/internal/rsync"
)

type Mount struct{}

func (r *Mount) Run(ctx context.Context, attempt *migration.Attempt, logger *slog.Logger) error {
	mig := attempt.Migration
	if reason := r.cannotDoReason(mig); reason != "" {
		return Declined(reason)
	}

	mover, err := buildMoverCmdLocal(mig)
	if err != nil {
		return fmt.Errorf("failed to build rsync command: %w", err)
	}

	sourceInfo := mig.SourceInfo
	vals := buildMountHelmValues(mig, mover)

	releaseName := attempt.HelmReleaseNamePrefix
	attempt.ReleaseNames = []string{releaseName}

	if err = installHelmChart(ctx, attempt, sourceInfo, releaseName, vals, logger); err != nil {
		return err
	}

	return waitForRsyncJob(ctx, attempt, sourceInfo, releaseName, logger)
}

// buildMountHelmValues builds the values for the one rsync pod that mounts both
// volumes. It copies between two filesystems and opens no connection, so it gets
// no network policy: one object less, and no permission to check for it.
func buildMountHelmValues(mig *migration.Migration, mover moverCommand) map[string]any {
	sourceInfo := mig.SourceInfo
	destInfo := mig.DestInfo

	component := map[string]any{
		keyEnabled:   true,
		keyNamespace: sourceInfo.Claim.Namespace,
		"nodeName":   determineTargetNode(mig),
		keyPVCMounts: []map[string]any{
			{
				keyName:      sourceInfo.Claim.Name,
				keyMountPath: srcMountPath,
				keyReadOnly:  !mig.Request.SourceMountReadWrite,
			},
			{
				keyName:      destInfo.Claim.Name,
				keyMountPath: destMountPath,
			},
		},
		keyAffinity:      sourceInfo.AffinityHelmValues,
		keyNetworkPolicy: map[string]any{keyEnabled: false},
	}

	maps.Copy(component, mover.values())

	return map[string]any{rsyncComponent: component}
}

func (r *Mount) cannotDoReason(t *migration.Migration) string {
	sourceInfo := t.SourceInfo
	destInfo := t.DestInfo

	sameCluster := sourceInfo.ClusterClient.RestConfig.Host == destInfo.ClusterClient.RestConfig.Host
	if !sameCluster {
		return "source and destination are on different clusters"
	}

	sameNamespace := sourceInfo.Claim.Namespace == destInfo.Claim.Namespace
	if !sameNamespace {
		return "source and destination are in different namespaces"
	}

	sameNode := sourceInfo.MountedNode == destInfo.MountedNode
	oneUnmounted := sourceInfo.MountedNode == "" || destInfo.MountedNode == ""

	if sameNode || oneUnmounted || sourceInfo.SupportsROX || sourceInfo.SupportsRWX ||
		destInfo.SupportsRWX {
		return ""
	}

	return "PVCs are mounted on different nodes and do not support multi-access modes"
}

func buildRsyncCmdMount(mig *migration.Migration) (string, error) {
	srcPath, destPath, err := resolveMountPaths(mig.Request)
	if err != nil {
		return "", err
	}

	rsyncCmd := rsync.Cmd{
		NoChown:  mig.Request.NoChown,
		NonRoot:  mig.Request.NonRoot,
		Delete:   mig.Request.DeleteExtraneousFiles,
		SrcPath:  srcPath,
		DestPath: destPath,
		// Both volumes are mounted in this one pod, so nothing crosses a
		// network and compressing only costs. rsync applies --compress to a
		// local transfer all the same: measured on 77.7 MiB it turned 0.12s
		// and 0.03s of CPU into 0.87s and 0.99s.
		Compress:  false,
		ExtraArgs: mig.Request.RsyncExtraArgs,
	}

	cmd, err := rsyncCmd.Build()
	if err != nil {
		return "", fmt.Errorf("failed to build rsync command: %w", err)
	}

	return cmd, nil
}

func determineTargetNode(t *migration.Migration) string {
	sourceInfo := t.SourceInfo
	destInfo := t.DestInfo

	if sourceInfo.MountedNode != "" && !sourceInfo.SupportsROX && !sourceInfo.SupportsRWX {
		return sourceInfo.MountedNode
	}

	if destInfo.MountedNode != "" && !destInfo.SupportsROX && !destInfo.SupportsRWX {
		return destInfo.MountedNode
	}

	if sourceInfo.MountedNode != "" {
		return sourceInfo.MountedNode
	}

	return destInfo.MountedNode
}
