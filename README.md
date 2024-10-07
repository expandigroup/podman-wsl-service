# Podman WSL Service

This tool is meant to run as a service in WSL2 and expose the Podman API to local WSL2 Docker/Podman clients.

It will transparently handle bind mounts from the local WSL2 distro into Podman containers.

Docker API clients such as the Docker CLI and Docker Compose should work with this service.

> [!WARNING]
> Podman Kubernetes pods are not supported and bind mounts will not be handled correctly.

## Building an executable

A single executable can be built with `pkg`.

```bash
npm run pkg
```

## Installation

Run `install.sh` as root to install the service.

```bash
sudo ./install.sh
```

Export the following environment variables in your shell:

```bash
DOCKER_HOST=unix:///run/podman/podman.sock
CONTAINER_HOST=unix:///run/podman/podman.sock
```

## License

Licensed under the MIT License.
