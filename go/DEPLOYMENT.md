# Production deployment

Production infrastructure for `https://niko.stream` is owned by
[NikoVonLas/infra](https://github.com/NikoVonLas/infra), node `v1144246`.
See `data/nodes/v1144246.yaml`, `profile::native_blog`, and
`docs/migrations/v1144246.md` there. This application repository owns the Go code
and build artifact; runtime configuration, TLS, firewall, backups and systemd units
belong to the infrastructure repository.
