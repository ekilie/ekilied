# ekilied

**`ekilied`** — Ekilie Cloud platform agent daemon. A lightweight Go binary that runs on your VPS and connects to the Ekilie Cloud control plane to manage sites, deployments, SSL certificates, and more.

## How it works

```
ekilie.cloud (API)  ◄──WSS──►  ekilied (on your VPS)
```

`ekilied` makes outbound connections only — no inbound ports needed beyond SSH.

## Quick start

```bash
curl -fsSL https://get.ekilie.cloud/install.sh | bash
```

## Build

```bash
make build
sudo cp build/ekilied /usr/local/bin/ekilied
```

## License

AGPL v3
