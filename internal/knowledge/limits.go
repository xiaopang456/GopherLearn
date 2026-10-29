package knowledge

// CheckCreation reports whether one new document fits: the count is refused at
// the ceiling, the byte total only past it. v2 applies both ceilings on upload
// (maxDocumentsPerUser, maxTotalSizeBytes), and the count check is what stops a
// user with a hundred empty-collection documents from adding a one-byte one.
//
// adding is a raw byte count; zero means "not measured", which is why Usage
// carries Unmeasured rather than pretending an unrecorded row is free space.
func (l Limits) CheckCreation(usage Usage, adding int64) error {
	if l.MaxDocuments > 0 && usage.DocumentCount >= int64(l.MaxDocuments) {
		return ErrDocumentCountLimit
	}
	return l.checkBytes(usage, adding, 0)
}

// CheckReplacement reports whether a rewrite fits. The document being rewritten
// already occupies one slot, so v2's update path measures only the storage: its
// own bytes are released before the new size is added.
func (l Limits) CheckReplacement(usage Usage, adding, removing int64) error {
	return l.checkBytes(usage, adding, removing)
}

func (l Limits) checkBytes(usage Usage, adding, removing int64) error {
	if l.MaxTotalBytes > 0 && usage.TotalBytes-removing+adding > l.MaxTotalBytes {
		return ErrStorageLimit
	}
	return nil
}
