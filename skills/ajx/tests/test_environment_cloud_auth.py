"""Synthetic cloud-auth contracts; no provider CLI, network, or credential discovery."""

import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "lib"))
from ajx import runner as runmod, spec as specmod
from ajx.base import Harness


CONFIG = {
    "claude-bedrock": {"AWS_REGION": "us-east-1"},
    "claude-vertex": {"CLOUD_ML_REGION": "us-central1", "ANTHROPIC_VERTEX_PROJECT_ID": "synthetic-project"},
    "claude-foundry": {"ANTHROPIC_FOUNDRY_RESOURCE": "synthetic-resource"},
    "gemini-vertex": {"GOOGLE_CLOUD_PROJECT": "synthetic-project", "GOOGLE_CLOUD_LOCATION": "us-central1"},
}
KEYS = {
    "claude-bedrock": {"AWS_ACCESS_KEY_ID": "synthetic-access", "AWS_SECRET_ACCESS_KEY": "synthetic-secret"},
    "claude-foundry": {"ANTHROPIC_FOUNDRY_API_KEY": "synthetic-foundry-key"},
    "gemini-vertex": {"GOOGLE_API_KEY": "synthetic-vertex-key"},
}
SERVICE_PRINCIPAL = {
    "AZURE_CLIENT_ID": "synthetic-client",
    "AZURE_TENANT_ID": "synthetic-tenant",
    "AZURE_CLIENT_SECRET": "synthetic-client-secret",
}
DIRECT = (
    ("claude-bedrock", {**CONFIG["claude-bedrock"], **KEYS["claude-bedrock"]}),
    ("claude-bedrock", {**CONFIG["claude-bedrock"], "AWS_BEARER_TOKEN_BEDROCK": "synthetic-bedrock-token"}),
    ("claude-foundry", {**CONFIG["claude-foundry"], **KEYS["claude-foundry"]}),
    ("claude-foundry", {**CONFIG["claude-foundry"], **SERVICE_PRINCIPAL}),
    ("gemini-vertex", {**CONFIG["gemini-vertex"], **KEYS["gemini-vertex"]}),
)
MARKERS = {
    "claude-bedrock": "CLAUDE_CODE_USE_BEDROCK",
    "claude-vertex": "CLAUDE_CODE_USE_VERTEX",
    "claude-foundry": "CLAUDE_CODE_USE_FOUNDRY",
    "gemini-vertex": "GOOGLE_GENAI_USE_VERTEXAI",
}


def inline_env(env):
    return "{ " + ", ".join(f"{key} = {json.dumps(value)}" for key, value in env.items()) + " }"


class EnvironmentCloudAuthTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.home = self.root / "caller-home"
        self.home.mkdir()
        (self.root / "task.md").write_text("Synthetic auth validation; do not call any provider.")
        environment = mock.patch.dict(os.environ, {"HOME": str(self.home)}, clear=True)
        environment.start()
        self.addCleanup(environment.stop)
        # A regression must not accidentally invoke a CLI while checking readiness.
        subprocess = mock.patch("subprocess.run", side_effect=AssertionError("unexpected subprocess"))
        subprocess.start()
        self.addCleanup(subprocess.stop)

    def trial(self, provider, *, backend="local", isolated=True, env=None, cell_env=None,
              auth_env=None, unset=(), configuration=False, auth_options=""):
        selected = 'environment = "worker"\n' if isolated else ""
        if configuration:
            selected += 'agent_configuration = "plain"\n'
        image = 'image = "sha256:' + "a" * 64 + '"\n' if backend == "container" else ""
        profile = '[agent_configurations.plain.skills]\nmode = "none"\n' if configuration else ""
        harness = "gemini-cli" if provider == "gemini-vertex" else "claude-code"
        path = self.root / "trial.toml"
        path.write_text(
            '[trial]\nid = "cloud-auth"\nproduct = "synthetic"\noutput_dir = "results"\n'
            + f'workspace_root = {json.dumps(str(self.root / "workspaces"))}\n'
            + selected
            + '[task]\nprompt_file = "task.md"\n'
            + '[env]\n' + "".join(f'{key} = {json.dumps(value)}\n' for key, value in (env or {}).items())
            + '[environments.worker]\n' + f'backend = "{backend}"\n' + image
            + profile
            + f'[auth.cloud]\ntype = "{provider}"\n'
            + f'env = {inline_env(auth_env or {})}\nunset = {json.dumps(list(unset))}\n'
            + auth_options
            + '[reporter]\nharness = "claude-code"\nauth = "inherit"\n'
            + f'[[cells]]\nid = "worker"\nharness = "{harness}"\nauth = "cloud"\n'
            + f'env = {inline_env(cell_env or {})}\n'
        )
        return path

    def load(self, provider, **options):
        spec = specmod.load(self.trial(provider, **options))
        cell = spec["cells"][0]
        return spec, cell, specmod.auth_for(spec, cell)

    def launch(self, spec, cell, auth):
        runner = specmod.runner_for(spec, cell)
        ctx = {
            "env": specmod.worker_env(spec, cell, auth), "unset_env": auth.unset(), "auth": auth,
            "environment_profile": specmod.environment_for(spec, cell), "runner": runner,
            "workspace": self.root, "run_dir": self.root, "timeout": 1,
        }
        with mock.patch.object(runner, "wrap", return_value=["synthetic-harness"]), \
                mock.patch("ajx.base.run_streaming", return_value={"exit_code": 0, "timed_out": False}) as launch:
            Harness().run(ctx, ["synthetic-harness"], "execute")
        return launch.call_args.kwargs["env"]

    def test_caller_home_login_and_profile_files_cannot_satisfy_validation(self):
        for relative in (".aws/credentials", ".config/gcloud/application_default_credentials.json",
                         ".azure/msal_token_cache.json"):
            path = self.home / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("synthetic cached login")
        os.environ.update(AWS_PROFILE="synthetic-profile", CLOUDSDK_CONFIG=str(self.home / ".config/gcloud"),
                          AZURE_CONFIG_DIR=str(self.home / ".azure"))
        for provider, config in CONFIG.items():
            for backend in ("local", "container"):
                with self.subTest(provider=provider, backend=backend):
                    with self.assertRaisesRegex(specmod.SpecError, "fresh HOME"):
                        self.load(provider, backend=backend, auth_env=config)

    def test_ambient_direct_credentials_do_not_authorize_forwarding(self):
        for values in KEYS.values():
            os.environ.update(values)
        os.environ.update(SERVICE_PRINCIPAL, AWS_SESSION_TOKEN="synthetic-session",
                          AWS_BEARER_TOKEN_BEDROCK="synthetic-bearer")
        for provider, config in CONFIG.items():
            with self.subTest(provider=provider):
                with self.assertRaisesRegex(specmod.SpecError, "direct-env|explicitly declared"):
                    self.load(provider, auth_env=config)

    def test_complete_direct_routes_work_from_every_declared_layer(self):
        for provider, values in DIRECT:
            for backend in ("local", "container"):
                for layer in ("env", "cell_env", "auth_env"):
                    with self.subTest(provider=provider, values=list(values), backend=backend, layer=layer):
                        spec, cell, auth = self.load(provider, backend=backend, **{layer: values})
                        self.assertEqual(auth.problems(), [])
                        self.assertEqual({k: specmod.worker_env(spec, cell, auth)[k] for k in values}, values)

    def test_auth_env_references_resolve_only_explicitly_selected_values(self):
        for provider, values in DIRECT:
            with self.subTest(provider=provider, values=list(values)):
                caller = {f"INPUT_{key}": value for key, value in values.items()}
                os.environ.update(caller, UNRELATED_TOKEN="synthetic-not-forwarded")
                declared = {key: "${INPUT_" + key + "}" for key in values}
                spec, cell, auth = self.load(provider, auth_env=declared)
                process = self.launch(spec, cell, auth)
                self.assertEqual({key: process[key] for key in values}, values)
                self.assertFalse(set(caller) & set(process))
                self.assertNotIn("UNRELATED_TOKEN", process)

    def test_required_config_variables_still_forward_without_forwarding_credentials(self):
        for provider, values in (("claude-bedrock", KEYS["claude-bedrock"]),
                                 ("gemini-vertex", KEYS["gemini-vertex"])):
            with self.subTest(provider=provider):
                os.environ.update(CONFIG[provider], AWS_SESSION_TOKEN="synthetic-ambient-session")
                spec, cell, auth = self.load(provider, auth_env=values)
                self.assertEqual(auth.problems(), [])
                process = self.launch(spec, cell, auth)
                self.assertEqual({key: process[key] for key in CONFIG[provider]}, CONFIG[provider])
                self.assertNotIn("AWS_SESSION_TOKEN", process)

    def test_partial_credentials_and_missing_route_fields_are_rejected(self):
        for provider, values in DIRECT:
            for missing in values:
                with self.subTest(provider=provider, missing=missing):
                    partial = {key: value for key, value in values.items() if key != missing}
                    with self.assertRaisesRegex(specmod.SpecError, missing):
                        self.load(provider, auth_env=partial)

    def test_empty_and_unresolved_values_do_not_fall_back_to_ambient(self):
        for provider, values in DIRECT:
            for name in values:
                for empty in ("", "   ", "${MISSING_SYNTHETIC_VALUE}"):
                    with self.subTest(provider=provider, name=name, empty=empty):
                        with mock.patch.dict(os.environ, values):
                            with self.assertRaisesRegex(specmod.SpecError, name):
                                self.load(provider, auth_env={**values, name: empty})
        alternatives = (
            ("claude-bedrock", {**CONFIG["claude-bedrock"], **KEYS["claude-bedrock"],
                               "AWS_BEARER_TOKEN_BEDROCK": "synthetic-bearer", "AWS_SESSION_TOKEN": "synthetic-session"}),
            ("claude-foundry", {**CONFIG["claude-foundry"], **KEYS["claude-foundry"], **SERVICE_PRINCIPAL,
                               "ANTHROPIC_FOUNDRY_BASE_URL": "https://foundry.example.test"}),
        )
        for provider, values in alternatives:
            for name in values:
                for invalid in ("   ", "${MISSING_SYNTHETIC_VALUE}"):
                    with self.subTest(provider=provider, alternate_route=True, name=name, invalid=invalid):
                        with self.assertRaisesRegex(specmod.SpecError, name):
                            self.load(provider, env={**values, name: invalid})

    def test_unresolved_worker_layer_credentials_are_not_literal_tokens(self):
        for provider, values in DIRECT:
            for layer in ("env", "cell_env"):
                with self.subTest(provider=provider, layer=layer):
                    with self.assertRaises(specmod.SpecError):
                        self.load(provider, **{layer: {key: "${MISSING_VALUE}" for key in values}})

    def test_exact_and_prefix_unset_remove_credentials_and_routing(self):
        for provider, values in DIRECT:
            for name in values:
                for unset in (name, name.rsplit("_", 1)[0] + "_*"):
                    with self.subTest(provider=provider, unset=unset, name=name):
                        with mock.patch.dict(os.environ, values):
                            with self.assertRaises(specmod.SpecError):
                                self.load(provider, auth_env=values, unset=[unset])

    def test_provider_selection_must_survive_unset_and_override(self):
        for provider, values in DIRECT:
            marker = MARKERS[provider]
            for override in ("", "0", "false", "${MISSING_MARKER}"):
                with self.subTest(provider=provider, override=override):
                    with self.assertRaisesRegex(specmod.SpecError, marker):
                        self.load(provider, auth_env={**values, marker: override})
            with self.subTest(provider=provider, unset=marker):
                with self.assertRaisesRegex(specmod.SpecError, marker):
                    self.load(provider, auth_env=values, unset=[marker])

    def test_foundry_accepts_base_url_with_key_or_complete_service_principal(self):
        for backend in ("local", "container"):
            for credentials in (KEYS["claude-foundry"], SERVICE_PRINCIPAL):
                with self.subTest(backend=backend, credentials=list(credentials)):
                    values = {**credentials, "ANTHROPIC_FOUNDRY_BASE_URL": "https://foundry.example.test/anthropic"}
                    spec, cell, auth = self.load("claude-foundry", backend=backend, auth_env=values)
                    self.assertEqual(auth.problems(), [])
                    process = self.launch(spec, cell, auth)
                    self.assertEqual({key: process[key] for key in values}, values)

    def test_foundry_ambient_endpoint_does_not_complete_an_explicit_key(self):
        os.environ.update(CONFIG["claude-foundry"], ANTHROPIC_FOUNDRY_BASE_URL="https://foundry.example.test")
        for credentials in (KEYS["claude-foundry"], SERVICE_PRINCIPAL):
            with self.subTest(credentials=list(credentials)):
                with self.assertRaisesRegex(specmod.SpecError, "ANTHROPIC_FOUNDRY_RESOURCE.*BASE_URL"):
                    self.load("claude-foundry", auth_env=credentials)

    def test_bedrock_session_token_is_forwarded_only_when_declared(self):
        values = {**CONFIG["claude-bedrock"], **KEYS["claude-bedrock"]}
        os.environ["AWS_SESSION_TOKEN"] = "synthetic-session"
        for declared in (False, True):
            with self.subTest(declared=declared):
                env = {**values, **({"AWS_SESSION_TOKEN": "${AWS_SESSION_TOKEN}"} if declared else {})}
                spec, cell, auth = self.load("claude-bedrock", auth_env=env)
                process = self.launch(spec, cell, auth)
                self.assertEqual(process.get("AWS_SESSION_TOKEN"), "synthetic-session" if declared else None)

    def test_explicit_credential_files_are_rejected_without_reading_or_staging(self):
        credential = self.home / "synthetic-private-login.json"
        credential.write_text("synthetic-private-file-content")
        original_open = Path.open

        def guarded_open(path, *args, **kwargs):
            if path == credential:
                self.fail("credential validation must not read credential files")
            return original_open(path, *args, **kwargs)

        cases = (
            ("claude-bedrock", "AWS_SHARED_CREDENTIALS_FILE"), ("claude-bedrock", "AWS_CONFIG_FILE"),
            ("claude-bedrock", "AWS_WEB_IDENTITY_TOKEN_FILE"),
            ("claude-vertex", "GOOGLE_APPLICATION_CREDENTIALS"), ("gemini-vertex", "GOOGLE_APPLICATION_CREDENTIALS"),
            ("claude-foundry", "AZURE_CLIENT_CERTIFICATE_PATH"), ("claude-foundry", "AZURE_FEDERATED_TOKEN_FILE"),
        )
        for provider, name in cases:
            for backend in ("local", "container"):
                for location in (str(credential), "relative-login.json", "/synthetic/missing-login.json"):
                    with self.subTest(provider=provider, name=name, backend=backend, location=location):
                        with mock.patch.object(Path, "open", guarded_open):
                            with self.assertRaisesRegex(specmod.SpecError, "custom auth plugin") as raised:
                                self.load(provider, backend=backend,
                                          auth_env={**CONFIG[provider], name: location})
                        self.assertIn(name, str(raised.exception))
                        self.assertIn("before archival", str(raised.exception))
                        self.assertNotIn(location, str(raised.exception))
        self.assertEqual(credential.read_text(), "synthetic-private-file-content")
        self.assertFalse((self.root / "workspaces").exists())
        self.assertFalse((self.root / "results").exists())

    def test_direct_credentials_do_not_hide_unsupported_file_or_login_selectors(self):
        cases = (
            ("claude-bedrock", "AWS_PROFILE"), ("claude-bedrock", "AWS_DEFAULT_PROFILE"),
            ("claude-bedrock", "AWS_SHARED_CREDENTIALS_FILE"),
            ("claude-bedrock", "AWS_CONTAINER_CREDENTIALS_FULL_URI"),
            ("claude-bedrock", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"),
            ("claude-bedrock", "CLAUDE_CODE_SKIP_BEDROCK_AUTH"),
            ("claude-foundry", "AZURE_CONFIG_DIR"), ("claude-foundry", "AZURE_TOKEN_CREDENTIALS"),
            ("claude-foundry", "ANTHROPIC_FOUNDRY_AUTH_TOKEN"),
            ("gemini-vertex", "GOOGLE_APPLICATION_CREDENTIALS"), ("gemini-vertex", "CLOUDSDK_CONFIG"),
            ("gemini-vertex", "GEMINI_API_KEY"),
        )
        for provider, selector in cases:
            with self.subTest(provider=provider, selector=selector):
                values = {**CONFIG[provider], **KEYS[provider], selector: "synthetic-unsupported-route"}
                with self.assertRaisesRegex(specmod.SpecError, selector):
                    self.load(provider, auth_env=values)
                spec, cell, auth = self.load(provider, auth_env=values, unset=[selector])
                self.assertEqual(auth.problems(), [])
                self.assertNotIn(selector, self.launch(spec, cell, auth))

    def test_unset_unused_reference_and_empty_selectors_do_not_block_a_direct_route(self):
        for provider, selector in (("claude-bedrock", "AWS_PROFILE"),
                                   ("claude-foundry", "AZURE_CONFIG_DIR"),
                                   ("gemini-vertex", "GOOGLE_APPLICATION_CREDENTIALS")):
            with self.subTest(provider=provider):
                values = {**CONFIG[provider], **KEYS[provider], selector: ""}
                _, _, auth = self.load(provider, auth_env=values)
                self.assertEqual(auth.problems(), [])
                values[selector] = "${MISSING_UNUSED_REFERENCE}"
                _, _, auth = self.load(provider, auth_env=values, unset=[selector])
                self.assertEqual(auth.problems(), [])

    def test_environment_launch_never_inherits_ambient_auth_or_config(self):
        ambient = {
            **{key: value for values in KEYS.values() for key, value in values.items()},
            **SERVICE_PRINCIPAL,
            "AWS_PROFILE": "synthetic-profile", "AWS_SESSION_TOKEN": "synthetic-session",
            "AWS_BEARER_TOKEN_BEDROCK": "synthetic-bearer",
            "GOOGLE_APPLICATION_CREDENTIALS": "/synthetic/caller-login.json",
            "CLOUDSDK_CONFIG": "/synthetic/gcloud", "AZURE_CONFIG_DIR": "/synthetic/azure",
            "ANTHROPIC_API_KEY": "synthetic-other-provider", "GEMINI_API_KEY": "synthetic-other-key",
            "HTTP_PROXY": "http://proxy.example.test", "UNRELATED_SECRET": "synthetic-unrelated",
        }
        os.environ.update(ambient)
        for provider, values in DIRECT:
            for backend in ("local", "container"):
                with self.subTest(provider=provider, route=list(values), backend=backend):
                    spec, cell, auth = self.load(provider, backend=backend, auth_env=values)
                    process = self.launch(spec, cell, auth)
                    self.assertEqual({name: process[name] for name in values}, values)
                    self.assertFalse((set(ambient) - set(values)) & set(process))
                    self.assertNotEqual(process["HOME"], str(self.home))

    def test_launch_rechecks_after_a_validated_credential_or_endpoint_is_removed(self):
        for provider, values in DIRECT:
            for name in values:
                with self.subTest(provider=provider, name=name):
                    spec, cell, auth = self.load(provider, auth_env=values)
                    runner = specmod.runner_for(spec, cell)
                    ctx = {"env": specmod.worker_env(spec, cell, auth), "auth": auth, "runner": runner,
                           "environment_profile": specmod.environment_for(spec, cell)}
                    with mock.patch.object(runner, "wrap") as wrap, \
                            mock.patch("ajx.base.run_streaming") as launch:
                        with self.assertRaisesRegex(RuntimeError, "authentication configuration is not ready"):
                            Harness().run(ctx, ["synthetic-harness"], "execute", extra_env={name: ""})
                    wrap.assert_not_called()
                    launch.assert_not_called()

    def test_launch_rechecks_harness_and_context_unset(self):
        for provider, values in DIRECT:
            spec, cell, auth = self.load(provider, auth_env=values)
            runner = specmod.runner_for(spec, cell)
            ctx = {"env": specmod.worker_env(spec, cell, auth), "auth": auth, "runner": runner,
                   "environment_profile": specmod.environment_for(spec, cell)}
            for drop_source in ("strip_env", "unset_env"):
                with self.subTest(provider=provider, drop_source=drop_source):
                    harness = Harness()
                    if drop_source == "strip_env":
                        harness.strip_env = tuple(values)
                    else:
                        ctx["unset_env"] = list(values)
                    with mock.patch.object(runner, "wrap") as wrap:
                        with self.assertRaisesRegex(RuntimeError, "authentication configuration is not ready"):
                            harness.run(ctx, ["synthetic-harness"], "execute")
                    wrap.assert_not_called()

    def test_prepare_rechecks_references_before_environment_provisioning(self):
        for index, (provider, values) in enumerate(DIRECT):
            with self.subTest(provider=provider, route=list(values)):
                with mock.patch.dict(os.environ, {"INPUT_" + key: value for key, value in values.items()}):
                    spec, cell, _ = self.load(
                        provider, auth_env={key: "${INPUT_" + key + "}" for key in values})
                    spec["trial"]["output_dir"] = str(self.root / f"prepare-results-{index}")
                    run = runmod.Run(spec, cell, 1, lambda _: None)
                with mock.patch.object(run.runner, "plan") as plan, \
                        mock.patch.object(run.runner, "prepare") as prepare, \
                        mock.patch.object(run.auth, "prepare") as stage_credentials:
                    with self.assertRaisesRegex(RuntimeError, "authentication configuration is not ready"):
                        run.stage_prepare()
                plan.assert_not_called()
                prepare.assert_not_called()
                stage_credentials.assert_not_called()

    def test_explicit_isolates_config_override_cannot_bypass_cloud_contract(self):
        for provider, config in CONFIG.items():
            with self.subTest(provider=provider):
                with self.assertRaisesRegex(specmod.SpecError, "fresh HOME"):
                    self.load(provider, auth_env=config, auth_options="isolates_config = true\n")

    def test_agent_configuration_implicit_environment_uses_same_auth_contract(self):
        for provider in ("claude-bedrock", "claude-vertex", "claude-foundry"):
            with self.subTest(provider=provider):
                with self.assertRaisesRegex(specmod.SpecError, "fresh HOME"):
                    self.load(provider, isolated=False, configuration=True, auth_env=CONFIG[provider])

    def test_claude_vertex_requires_custom_staging_even_with_project_and_region(self):
        for extra in ({}, {"GOOGLE_APPLICATION_CREDENTIALS": "/synthetic/declared-credentials.json"},
                      {"ANTHROPIC_AUTH_TOKEN": "synthetic-gateway-token", "CLAUDE_CODE_SKIP_VERTEX_AUTH": "1"}):
            with self.subTest(extra=list(extra)):
                with self.assertRaisesRegex(specmod.SpecError, "no supported direct-env credential route") as raised:
                    self.load("claude-vertex", auth_env={**CONFIG["claude-vertex"], **extra})
                self.assertIn("type='env'", str(raised.exception))
                self.assertIn("before archival", str(raised.exception))

    def test_diagnostics_and_descriptions_do_not_expose_credential_values(self):
        for provider, values in DIRECT:
            with self.subTest(provider=provider, route=list(values)):
                _, _, auth = self.load(provider, auth_env=values)
                for value in values.values():
                    self.assertNotIn(value, json.dumps(auth.describe()))
                invalid = {**values, MARKERS[provider]: "false"}
                with self.assertRaises(specmod.SpecError) as raised:
                    self.load(provider, auth_env=invalid)
                for value in values.values():
                    self.assertNotIn(value, str(raised.exception))

    def test_legacy_runs_preserve_caller_credentials_and_home_behavior(self):
        os.environ.update(AWS_PROFILE="synthetic-profile", AWS_SESSION_TOKEN="synthetic-session",
                          GOOGLE_APPLICATION_CREDENTIALS="/synthetic/legacy-adc.json",
                          AZURE_CONFIG_DIR="/synthetic/legacy-azure")
        for provider, config in CONFIG.items():
            with self.subTest(provider=provider):
                spec, cell, auth = self.load(provider, isolated=False, auth_env=config)
                self.assertIsNone(auth.environment_check)
                self.assertEqual(auth.problems(), [])
                process = self.launch(spec, cell, auth)
                self.assertEqual(process["HOME"], str(self.home))
                self.assertEqual(process["AWS_PROFILE"], "synthetic-profile")
                self.assertEqual(process["AWS_SESSION_TOKEN"], "synthetic-session")

    def test_host_reporter_does_not_inherit_worker_auth_readiness_binding(self):
        spec, _, _ = self.load("claude-bedrock", auth_env={**CONFIG["claude-bedrock"], **KEYS["claude-bedrock"]})
        spec["auth"]["cloud"]["env"] = dict(CONFIG["claude-bedrock"])
        reporter = specmod.auth_for(spec, {"id": "reporter", "harness": "claude-code", "auth": "cloud"})
        self.assertIsNone(reporter.environment_check)
        self.assertEqual(reporter.problems(), [])

    def test_generic_auth_retains_custom_contract_escape_hatch(self):
        spec, cell, auth = self.load("env", auth_env={"SYNTHETIC_GATEWAY_KEY": "synthetic-key"},
                                     auth_options="isolates_config = true\n")
        self.assertEqual(auth.problems(), [])
        self.assertEqual(self.launch(spec, cell, auth)["SYNTHETIC_GATEWAY_KEY"], "synthetic-key")


if __name__ == "__main__":
    unittest.main()
