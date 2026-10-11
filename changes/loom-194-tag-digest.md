### Fixed

- Every tag of a published image names the pushed, attested digest (LOOM-194). The first release run's re-tagging wrapped the image in a manifest list, so `:X.Y.Z` and `:main` had a digest of their own and `gh attestation verify` by those tags failed; tags are now set by copying the manifest as it is.
