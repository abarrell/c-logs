# compose-logs

An interactive TUI for streaming Docker logs. Toggle services on/off, scroll through history, and pretty-print JSON structured logs — all from your terminal.

Works with **Docker Compose** and **plain Docker containers**. Auto-detects which mode to use: if a compose file exists in the current or parent directory, it uses Compose; otherwise it falls back to listing all Docker containers.

## Install

```bash
go install github.com/abarrell/compose-logs@latest
```

## Usage

Run from any directory. If a compose file is found, services come from Compose. Otherwise, all Docker containers are listed:

```bash
compose-logs                  # start with all running services active
compose-logs web api          # start with specific services active
compose-logs -n 200           # show last 200 lines of history per service
```

## Controls

| Key | Action |
|---|---|
| `1-9`, `0` | Toggle service by number |
| `a` | Activate all services |
| `n` | Deactivate all services |
| `r` | Activate only running services |
| `p` | Toggle JSON pretty-print |
| `c` | Clear log output |
| `↑`/`k` | Scroll up |
| `↓`/`j` | Scroll down |
| `PgUp`/`PgDn` | Scroll by half page |
| `G`/`End` | Jump to bottom (resume auto-scroll) |
| `q` | Quit |

Mouse wheel scrolling is also supported.

## JSON Pretty-Print

Structured JSON logs (e.g. Go `slog`, `zerolog`) are automatically detected and formatted:

**Pretty mode (`p` on):**
```
  api │ INF executing query
      │   duration=12ms
      │   query=SELECT * FROM users
```

**Compact mode (`p` off):**
```
  api │ INF executing query  duration=12ms  query=SELECT * FROM users
```

Nested JSON values are indented and unicode escapes (e.g. `\u0026`) are resolved in both modes.

## Requirements

- Go 1.24+
- Docker (with or without Compose)
