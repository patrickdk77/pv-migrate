package bucketstorage

import (
	"encoding/base64"
	"fmt"
	"time"

	"go.yaml.in/yaml/v4"
)

// metadataSuffix is appended to the backup name to form the sidecar's name.
const metadataSuffix = ".meta.yaml"

// metadataValues carries the rendered sidecar and where it is written. At most
// one of remotePath and localPath is set: a bucket backup uploads it, an
// archive backup writes it next to the archive file.
type metadataValues struct {
	base64     string
	remotePath string
	localPath  string
}

// Metadata holds information about a backup stored alongside the data in the
// bucket, or next to the archive file on the archive claim.
type Metadata struct {
	Version         int       `yaml:"version"`
	BackupTime      time.Time `yaml:"backupTime"`
	SourceNamespace string    `yaml:"sourceNamespace"`
	SourcePVC       string    `yaml:"sourcePvc"`
	// Format and Compression are set for an archive backup and empty for a
	// bucket one, where the data is stored as the files themselves.
	Format      string `yaml:"format,omitempty"`
	Compression string `yaml:"compression,omitempty"`
}

func generateMetadataBase64(namespace, pvcName, format, compression string) (string, error) {
	meta := Metadata{
		Version:         1,
		BackupTime:      time.Now().UTC(),
		SourceNamespace: namespace,
		SourcePVC:       pvcName,
		Format:          format,
		Compression:     compression,
	}

	data, err := yaml.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("failed to marshal backup metadata: %w", err)
	}

	return base64.StdEncoding.EncodeToString(data), nil
}
