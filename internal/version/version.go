package version

const Version = "0.7.2"

// IndexVersion is incremented whenever the index schema or parser changes in a
// way that requires a full rebuild. Bump this alongside Version when releasing
// a change that makes existing indexes stale — and bump daemon.ContractVersion
// in the same release, because a running daemon from the older build would
// otherwise keep serving the new frontend from that stale index.
const IndexVersion = 14
