"""Bounded direct-env contracts for cloud auth in explicit execution environments.

No provider discovery, credential-file reads, staging, or ambient credential forwarding.
Legacy runs retain each provider's existing credential-chain behavior.
"""

import re

from .base import Auth


def _matches(name, patterns):
    return any(name.startswith(p[:-1]) if p.endswith("*") else name == p for p in patterns)


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

    def problems(self, env=None):
        if env is None and self.environment_check is not None:
            return self.environment_check()
        return super().problems(env)

    def environment_problems(self, env, environment):
        unset = self.unset()
        env = {k: v for k, v in env.items() if not _matches(k, unset)}
        out = super().problems(env)
        route_names = tuple(name for route in self.credential_routes for name in route)
        for name in (*self.required_env, *route_names, *self.endpoint_routes, *self.optional_env):
            if env.get(name) and not _supplied(env, name):
                out.append(f"env var {name} must be non-empty and resolved in the worker environment")
        if self.provider_env and env.get(self.provider_env, "").lower() not in ("1", "true"):
            out.append(f"set {self.provider_env}=1 (or true) and do not unset it")
        if self.endpoint_routes and not any(_supplied(env, name) for name in self.endpoint_routes):
            out.append("declare " + " or ".join(self.endpoint_routes) + " in the worker env to select the endpoint")

        unsupported = sorted(k for k, v in env.items() if v and _matches(k, self.unsupported_env))
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
