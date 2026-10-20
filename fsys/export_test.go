package fsys

// Exports for the external test package.
var (
	ShQuote          = shQuote
	SSHVersion       = sshVersion
	HeredocDelimiter = heredocDelimiter
	SSHPrelude       = sshPrelude
)

// KillSSHSession kills the ssh process of f behind its back, like a dropped
// connection would.
func KillSSHSession(f FS) { f.(*sshFS).abort() }
