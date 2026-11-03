package environments

import "regexp"

var (
	nameRE        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	envNameRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	imageIDRE     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	imageRE       = regexp.MustCompile(`^(?:sha256:[0-9a-f]{64}|[a-z0-9][a-z0-9._:/-]*@sha256:[0-9a-f]{64})$`)
	containerIDRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	ownerRE       = regexp.MustCompile(`^[0-9a-f]{32}$`)
	platformRE    = regexp.MustCompile(`^linux/(?:amd64|arm64|arm|386|ppc64le|s390x|riscv64)(?:/v[0-9]+)?$`)
	workerUserRE  = regexp.MustCompile(`^[1-9][0-9]*:[1-9][0-9]*$`)
	secretNameRE  = regexp.MustCompile(`(?i)TOKEN|SECRET|PASSWORD|CREDENTIAL|(?:^|_)KEY$`)
	vmMountError  = regexp.MustCompile(`(?i)bind source path does not exist|mounts denied|not shared from the host`)
)

// ContainerPath is the fixed PATH inside containers and for container probes.
const ContainerPath = "/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin"

const (
	containerPath = ContainerPath
	managerPath   = "/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin"
	labelPrefix   = "io.ajx.environment."
	probeTarget   = "/ajx-probe"
	maxFiles      = 10000
	// VMMountHint is appended to Docker errors a VM-based engine produces for unshared directories.
	VMMountHint = "VM-based Docker engines (Colima, Docker Desktop, Rancher Desktop) can only bind-mount shared " +
		"directories; set trial.workspace_root to a directory the engine shares " +
		"(for example under your home directory)"
)

// maxBytes bounds a starting directory (a variable so tests can lower it).
var maxBytes int64 = 64 * 1024 * 1024

// reinspectDelay is the pause before one fresh re-inspection, in seconds.
const reinspectDelay = 0.5

var baseEnv = map[string]string{"PATH": containerPath, "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8",
	"TZ": "UTC", "SHELL": "/bin/sh", "TERM": "dumb", "HOME": "/nonexistent"}

// StopWorkers runs only through Docker inside an ownership-checked, private
// PID namespace. The non-root control process cannot signal the root-owned
// lifetime supervisor.
const StopWorkers = `
for attempt in 1 2 3 4 5 6 7 8 9 10; do
    live=
    for entry in /proc/[0-9]*/status; do
        pid=${entry#/proc/}
        pid=${pid%/status}
        [ "$pid" = "$$" ] && continue
        uid= state=
        if ! {
            while read -r key value rest; do
                case "$key" in Uid:) uid=$value;; State:) state=$value;; esac
            done
        } 2>/dev/null < "$entry"; then
            [ -e "$entry" ] && exit 1
            continue
        fi
        [ "$uid" = "$1" ] || continue
        case "$state" in Z|X) continue;; esac
        live=1
        kill -s KILL "$pid" 2>/dev/null || :
    done
    [ -z "$live" ] && exit 0
    /bin/sleep 0.05 || exit 1
done
exit 1
`

// Roots are the four per-attempt private directories, in order.
var Roots = []string{"workspace", "config_dir", "cache_dir", "home_dir"}

type limitSpec struct {
	key      string
	def      float64
	low      float64
	high     float64
	integer  bool
	defIsInt bool
}

var limitSpecs = []limitSpec{
	{"cpus", 1.0, 0.01, 1024, false, false},
	{"memory_mb", 1024, 16, 1048576, true, true},
	{"pids", 256, 16, 65536, true, true},
	{"tmpfs_mb", 64, 1, 1048576, true, true},
	{"lifetime_seconds", 3600, 1, 86400, true, true},
}

// RequiredCapabilities are the capability names a profile may require.
var RequiredCapabilities = []string{"filesystem_isolation", "lifetime", "network_isolation", "non_root", "resource_limits"}

var profileKeys = setOf("id", "backend", "image", "platform", "network", "tools", "mounts",
	"required", "permissions", "limits", "docker_host")

var isolationEnv = setOf("HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
	"TMPDIR", "TMP", "TEMP", "PYTHONUSERBASE", "npm_config_cache", "npm_config_prefix",
	"CARGO_HOME", "RUSTUP_HOME", "GOPATH", "GOCACHE", "PIP_CACHE_DIR", "UV_CACHE_DIR")

var unsafeEnv = setOf("ENV", "BASH_ENV", "CDPATH", "GLOBIGNORE", "SHELLOPTS", "BASHOPTS", "PROMPT_COMMAND",
	"PYTHONHOME", "PYTHONPATH", "SSH_AUTH_SOCK", "SSH_AGENT_PID")

var unsafePrefixes = []string{"LD_", "DYLD_", "DOCKER_", "BASH_FUNC_"}

var hostDrop = union(unsafeEnv, setOf(
	"PYTHONSTARTUP", "PYTHONINSPECT", "PYTHONSAFEPATH", "PYTHONNOUSERSITE", "PYTHONPYCACHEPREFIX",
	"NODE_OPTIONS", "NODE_PATH", "RUBYOPT", "RUBYLIB", "PERL5OPT", "PERL5LIB",
	"GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_CONFIG_NOSYSTEM",
	"GIT_SSH", "GIT_SSH_COMMAND", "GIT_ASKPASS", "SSH_ASKPASS", "GIT_PROXY_COMMAND",
	"VIRTUAL_ENV", "CONDA_PREFIX", "CONDA_DEFAULT_ENV", "PIP_TARGET", "PIP_PREFIX",
	"BUNDLE_GEMFILE", "R_PROFILE", "R_PROFILE_USER", "R_ENVIRON", "R_ENVIRON_USER",
	"OPENSSL_CONF", "OPENSSL_MODULES", "OPENSSL_ENGINES"))

var hostDropPrefixes = append(append([]string{}, unsafePrefixes...), "GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_", "_PYTHON_")

var hostExternalFiles = setOf(
	"AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_WEB_IDENTITY_TOKEN_FILE",
	"GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_CONFIG", "AZURE_CONFIG_DIR",
	"BOTO_CONFIG", "BOTO_PATH", "KUBECONFIG", "NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE",
	"SSL_CERT_DIR", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE")

var forbiddenNames = setOf(
	".aws", ".ssh", ".docker", ".kube", ".azure", ".gcloud", ".config", ".cache",
	".codex", ".claude", ".kiro", ".git", ".ajx-local", ".env", ".netrc",
	"ajx-reports", "reports", "credentials", "credentials.json", "auth.json",
	"state.json", "run.json", "report.html", "measurements.json")

var systemTargets = []string{"/bin", "/sbin", "/usr", "/lib", "/lib64", "/etc", "/proc", "/sys",
	"/dev", "/run", "/root", "/boot"}

func setOf(items ...string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, it := range items {
		out[it] = true
	}
	return out
}

func union(a, b map[string]bool) map[string]bool {
	out := make(map[string]bool, len(a)+len(b))
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if len(s) >= len(p) && s[:len(p)] == p {
			return true
		}
	}
	return false
}
