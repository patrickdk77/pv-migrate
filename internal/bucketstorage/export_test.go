package bucketstorage

var (
	BuildRemotePath      = buildRemotePath
	BuildHelmValues      = buildHelmValues
	MergeHelmValues      = mergeHelmValues
	ShouldUploadMetadata = shouldUploadMetadata
	ValidateSubpath      = validateSubpath
)

// MetadataValues builds the unexported carrier so the external test can pass
// one to BuildHelmValues.
func MetadataValues(base64, remotePath, localPath string) metadataValues {
	return metadataValues{base64: base64, remotePath: remotePath, localPath: localPath}
}
