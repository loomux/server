package targets

// CurrentManaged exposes the managed SSH configuration to external tests.
var CurrentManaged = currentManaged

// SetSSHConfigForTest points ResolveSSHConfig at a config file and an
// ~/.ssh stand-in instead of the real ones; it returns the undo.
func SetSSHConfigForTest(configFile, dir string) (restore func()) {
	oldFile, oldDir := sshConfigFile, sshDir
	sshConfigFile, sshDir = configFile, func() (string, error) { return dir, nil }
	return func() { sshConfigFile, sshDir = oldFile, oldDir }
}
