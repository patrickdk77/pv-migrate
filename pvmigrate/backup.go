package pvmigrate

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/utkuozdemir/pv-migrate/internal/bucketstorage"
	"github.com/utkuozdemir/pv-migrate/internal/opid"
	"github.com/utkuozdemir/pv-migrate/internal/rclone"
)

// Backup holds all configuration for a PVC backup to bucket storage.
type Backup struct {
	// ID is an optional custom identifier. When empty, a petname-style identifier
	// is generated automatically.
	ID string

	// ImageTag is the Docker image tag for the rclone container.
	// When non-empty, it is injected as the lowest-priority Helm value.
	ImageTag string

	// ChartVersion overrides the embedded Helm chart version metadata.
	ChartVersion string

	PVC PVC

	// Backend is the storage backend: "s3", "azure", or "gcs".
	Backend string
	// Bucket is the bucket (or container) name.
	Bucket string

	// S3-specific options
	S3Provider string
	Endpoint   string
	Region     string
	AccessKey  string
	SecretKey  string

	// Azure-specific options
	StorageAccount string
	StorageKey     string

	// GCS-specific options
	GCSServiceAccountJSON string
	// GCSBucketPolicyOnly controls rclone's bucket_policy_only setting.
	// Nil uses rclone config generation's default of true.
	GCSBucketPolicyOnly *bool

	// Name is the backup identity in the bucket. Required.
	Name string
	// Prefix is the global prefix in the bucket (default: pv-migrate).
	Prefix string
	// Path is a subdirectory inside the PVC to back up instead of the entire volume.
	Path string

	// RcloneConfigFile is the path to a raw rclone.conf file (power-user escape hatch).
	RcloneConfigFile string
	// Remote is the remote spec for raw config mode (e.g., "myremote:bucket/path").
	Remote string

	// RcloneExtraArgs are extra flags appended to the rclone command after the built-in progress flags.
	RcloneExtraArgs string

	// ArchiveFile selects the archive workflow: the volume is written to or
	// read from one tar file instead of being synced to a bucket. Its value is
	// "<claim>:<path>" for a file on a claim, or a bare path for one on this
	// process's own filesystem. Compression comes from the path's extension,
	// and strftime-style tokens in it are expanded when the operation runs.
	ArchiveFile string
	// CompressionLevel overrides the compressor's own default. Zero uses it.
	CompressionLevel int

	// Snapshot cuts a VolumeSnapshot of the claim and backs up a clone of it,
	// so the backup reads a point in time rather than the live volume. The
	// clone and the snapshot are removed afterwards unless KeepSnapshot is
	// set. SnapshotClass names the VolumeSnapshotClass; empty uses the
	// cluster default.
	Snapshot      bool
	SnapshotClass string
	KeepSnapshot  bool
	// FromSnapshot names an existing VolumeSnapshot to clone and back up,
	// instead of cutting a new one.
	FromSnapshot string
	// Flush quiesces the database in the pod that has the claim mounted while
	// the snapshot is cut. It names the kind of database, and implies
	// Snapshot. FlushContainer picks the container in a multi-container pod,
	// and FlushCommand replaces the client command the kind would run.
	Flush          string
	FlushContainer string
	FlushCommand   []string

	IgnoreMounted      bool
	NonRoot            bool
	Detach             bool
	NoCleanup          bool
	NoCleanupOnFailure bool

	HelmTimeout      time.Duration
	HelmValuesFiles  []string
	HelmValues       []string
	HelmFileValues   []string
	HelmStringValues []string

	Writer io.Writer
	Logger *slog.Logger

	// StructuredLogs reports that Logger writes machine-readable records to the
	// same stream as Writer. Set it to keep plain-text blocks such as failure
	// diagnostics out of that stream; the same information is logged instead.
	StructuredLogs bool

	// ColorOutput colors the plain-text report blocks semantically. Set it only
	// when Writer is a terminal.
	ColorOutput bool
}

// RunBackup executes the backup.
func RunBackup(ctx context.Context, backup Backup) error {
	applyBackupDefaults(&backup)

	if backup.ID != "" {
		if err := opid.Validate(backup.ID); err != nil {
			return err
		}
	}

	req := toBackupRequest(&backup, rclone.DirectionBackup)

	if err := bucketstorage.Run(ctx, req); err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}

	return nil
}

func applyBackupDefaults(backup *Backup) {
	if backup.Prefix == "" {
		backup.Prefix = DefaultPrefix
	}

	if backup.HelmTimeout == 0 {
		backup.HelmTimeout = defaultHelmTimeout
	}

	if backup.Writer == nil {
		backup.Writer = os.Stderr
	}

	if backup.Logger == nil {
		backup.Logger = slog.New(slog.DiscardHandler)
	}
}

func toBackupRequest(backup *Backup, direction string) *bucketstorage.Request {
	return &bucketstorage.Request{
		ID:                    backup.ID,
		ImageTag:              backup.ImageTag,
		ChartVersion:          backup.ChartVersion,
		Direction:             direction,
		KubeconfigPath:        backup.PVC.KubeconfigPath,
		Context:               backup.PVC.Context,
		Namespace:             backup.PVC.Namespace,
		PVCName:               backup.PVC.Name,
		IgnoreMounted:         backup.IgnoreMounted,
		NonRoot:               backup.NonRoot,
		Detach:                backup.Detach,
		NoCleanup:             backup.NoCleanup,
		NoCleanupOnFailure:    backup.NoCleanupOnFailure,
		Backend:               backup.Backend,
		Bucket:                backup.Bucket,
		S3Provider:            backup.S3Provider,
		Endpoint:              backup.Endpoint,
		Region:                backup.Region,
		AccessKey:             backup.AccessKey,
		SecretKey:             backup.SecretKey,
		StorageAccount:        backup.StorageAccount,
		StorageKey:            backup.StorageKey,
		GCSServiceAccountJSON: backup.GCSServiceAccountJSON,
		GCSBucketPolicyOnly:   backup.GCSBucketPolicyOnly,
		Name:                  backup.Name,
		Prefix:                backup.Prefix,
		Path:                  backup.Path,
		RcloneConfigFile:      backup.RcloneConfigFile,
		Remote:                backup.Remote,
		RcloneExtraArgs:       backup.RcloneExtraArgs,
		ArchiveFile:           backup.ArchiveFile,
		CompressionLevel:      backup.CompressionLevel,
		Snapshot:              backup.Snapshot,
		SnapshotClass:         backup.SnapshotClass,
		KeepSnapshot:          backup.KeepSnapshot,
		FromSnapshot:          backup.FromSnapshot,
		Flush:                 backup.Flush,
		FlushContainer:        backup.FlushContainer,
		FlushCommand:          backup.FlushCommand,
		HelmTimeout:           backup.HelmTimeout,
		HelmValuesFiles:       backup.HelmValuesFiles,
		HelmValues:            backup.HelmValues,
		HelmFileValues:        backup.HelmFileValues,
		HelmStringValues:      backup.HelmStringValues,
		Writer:                backup.Writer,
		StructuredLogs:        backup.StructuredLogs,
		ColorOutput:           backup.ColorOutput,
		Logger:                backup.Logger,
	}
}
