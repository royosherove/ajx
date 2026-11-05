"""Bounded direct-env contracts for cloud auth in explicit execution environments.

No provider discovery, credential-file reads, staging, or ambient credential forwarding.
Legacy runs retain each provider's existing credential-chain behavior.
"""

import re

from .base import Auth
from .util import env_name_matches


def _supplied(env, name):
    value = env.get(name, "")
    return bool(value.strip()) and not re.search(r"\$\{[A-Za-z_][A-Za-z0-9_]*\}", value)


class CloudAuth(Auth):
    """Subclasses declare complete supported routes; other routes need a custom auth plugin."""

    provider_env = None
    credential_routes = ()
    optional_env = ()
    endpoint_routes = ()
    unsupported_env = ()
    home_credentials = "caller-home credentials"
    # (declared, alias) pairs. When the profile's env sets the first name and nothing sets the second,
    # the worker gets the alias with the same value: Claude Code and the AWS CLI read AWS_REGION, but
    # botocore (a pip-installed boto3 in a setup script) reads only AWS_DEFAULT_REGION.
    env_aliases = (("AWS_REGION", "AWS_DEFAULT_REGION"),)

    def env(self):
        env = super().env()
        for declared, alias in self.env_aliases:
            value = env.get(declared, "")
            if value.strip() and alias not in env and not env_name_matches(alias, self.unset()):
                env[alias] = value
        return env

    def model_vars(self, env=None):
        """A mirrored alias is the model's variable too, so envcheck can tell it from a task variable."""
        declared = self.conf.get("env") or {}
        return super().model_vars(env) + tuple(alias for name, alias in self.env_aliases if name in declared)

    def problems(self, env=None):
        if env is None and self.environment_check is not None:
            return self.environment_check()
        return super().problems(env)

    def environment_problems(self, env, environment):
        unset = self.unset()
        env = {k: v for k, v in env.items() if not env_name_matches(k, unset)}
        out = super().problems(env)
        route_names = tuple(name for route in self.credential_routes for name in route)
        for name in (*self.required_env, *route_names, *self.endpoint_routes, *self.optional_env):
            if env.get(name) and not _supplied(env, name):
                out.append(f"env var {name} must be non-empty and resolved in the worker environment")
        if self.provider_env and env.get(self.provider_env, "").lower() not in ("1", "true"):
            out.append(f"set {self.provider_env}=1 (or true) and do not unset it")
        if self.endpoint_routes and not any(_supplied(env, name) for name in self.endpoint_routes):
            out.append("declare " + " or ".join(self.endpoint_routes) + " in the worker env to select the endpoint")

        unsupported = sorted(k for k, v in env.items() if v and env_name_matches(k, self.unsupported_env))
        if unsupported:
            out.append(
                f"{self.name} does not support {', '.join(unsupported)} in explicit environments; "
                "remove/unset these controls and use a supported direct-env route, or use a custom auth plugin "
                "that supplies credentials inside the selected environment and removes them before archival. "
                "A caller file path is not staged or made available in a container.")
        if not self.credential_routes:
            out.append(
                f"{self.name} has no supported direct-env credential route in explicit environments: "
                f"{self.home_credentials} are not provisioned for a fresh HOME. Use a custom auth plugin that "
                "stages credentials inside the selected environment and removes them before archival; "
                "a preconfigured gateway can use type='env' with its complete explicit auth contract.")
        elif not any(all(_supplied(env, name) for name in route) for route in self.credential_routes):
            routes = " or ".join(" + ".join(route) for route in self.credential_routes)
            out.append(
                f"{self.name} requires explicitly declared {routes} in the worker env "
                f"(auth.env supports ${{VAR}} references); {self.home_credentials} and ambient credentials "
                "are not supported by this contract for a fresh HOME. Other routes require type='env' with a complete "
                "independently supplied contract or a custom auth plugin with staging and cleanup.")
        return out
