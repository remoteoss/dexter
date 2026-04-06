package version

const Version = "0.5.1"

// IndexVersion is incremented whenever the index schema or parser changes in a
// way that requires a full rebuild. Bump this alongside Version when releasing
// a change that makes existing indexes stale.
const IndexVersion = 7
