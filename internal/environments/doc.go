// Package environments ports lib/ajx/environments.py: explicit local and
// container worker environments (standard library only).
//
// Public contract:
//
//	NormalizeProfiles(raw, baseDir) -> {name: normalized profile}; errors are *Error (Kind "ValueError").
//	ValidateForCell(profile, cell) -> error (Kind "ValueError").
//	New / NewEnvironmentRunner(profile) -> *EnvironmentRunner (plugin.ManagedRunner,
//	  plugin.CheckedManagedRunner): Plan, Prepare, Doctor, Describe, EnvironmentEnv,
//	  ChildEnv, HostEnv, Wrap, Shell, RestoreWorkerOwnership, Abort, Release.
//
// Persist Plan VERBATIM in ctx.State["environment"] before Prepare. Store
// Prepare's report separately (state["environment_report"]). Its fields
// include status, image, platform, inventory, capabilities, mounts, env_names
// and limitations. Inventory has phase ("initial" or "recovered") and tools;
// each declared tool probe records its exit status and bounded output.
// Capability entries distinguish declared policy from state/mechanism/observations.
//
// Profiles under [environments.NAME] accept only id (optional; must equal
// NAME), backend ("local" or "container"), image (container only: a full
// sha256 image ID or repository@sha256:digest), platform (optional Linux
// os/arch[/variant]), network, tools = [{name, argv, required}],
// mounts = [{source, target, access = "read" | "write"}], required
// (capability names), permissions = {filesystem, root, package_install},
// limits = {cpus, memory_mb, pids, tmpfs_mb, lifetime_seconds} and
// docker_host (a local unix:// socket; never a remote daemon). The normalized
// _base_dir field is internal, not a TOML setting.
//
// The local backend is NOT a filesystem, network, or sudo sandbox: required
// isolation is rejected. The container backend starts one persistent
// container per attempt with a non-root worker, a root-owned bounded
// supervisor, --init, --rm, a read-only rootfs, no capabilities and
// no-new-privileges, from an image that must already be present (tags are
// rejected). Ownership is persisted before creation; only the runner instance
// that made a fresh plan can create its container, once. Recovered instances
// may attach to an existing, matching, running container; they never create,
// restart, or replay a missing or expired attempt. Ownership labels are
// checked before removal, then removal uses the full immutable container ID.
//
// Errors that Python raises are returned. The plain plugin.ManagedRunner
// methods without an error result (ChildEnv, EnvironmentEnv, HostEnv, Shell)
// never fall back to the caller's environment: on error they return an empty
// environment or a failed record; the *Checked methods (and
// plugin.ContextShellChecked) return the error itself.
package environments
