# Windows local service template

This directory contains a template only. It does not install a service.

1. Copy the project to a restricted directory.
2. Set `M365_ADMIN_PASSWORD` through a protected service environment mechanism, or use `M365_ADMIN_PASSWORD_BOOTSTRAP_FILE`.
3. Keep `M365_LISTEN=127.0.0.1:4141` unless a separately authenticated reverse proxy is in place.
4. Register `bin\pc-self-ms.exe` using the organization's approved service manager.
5. Grant the service account ACL access only to the application and data directories.
6. Do not place access or refresh tokens in this template or in source control.
