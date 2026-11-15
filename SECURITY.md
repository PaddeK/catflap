# Security policy

catflap guards access to your homelab, so security reports are very welcome.

## Reporting a vulnerability

Please **don't open a public issue**. Report it privately through GitHub instead:
[Security → Report a vulnerability](https://github.com/PaddeK/catflap/security/advisories/new).

Helpful details: the catflap version (`catflap version`), which role is affected (relay,
homelab or client), and how to reproduce it. You'll get an answer as soon as possible; a
fix ships as a new release, with an advisory once it's out.

## Supported versions

Only the latest release gets fixes. Upgrade by running the install script again
(see the [README](README.md#install)).

## Scope

The security model is described in [docs/design.md](docs/design.md#security-model).
Problems in tailcat or Tailscale's DERP code belong upstream
([tailscale/tailscale security](https://github.com/tailscale/tailscale/security)), but tell us too if catflap is
affected.
