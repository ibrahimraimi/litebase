# CLI Installation

Because Litebase is built purely in Go, compiling the CLI globally onto your machine is quick and straightforward.

## Installation via Source
From the `packages/cli` directory, build the root module:

```bash
cd packages/cli
go build -o litebase .
```

Move the executable into a location exposed via your system `$PATH`, such as `~/.local/bin` or `~/go/bin`.

```bash
mkdir -p ~/.local/bin
cp litebase ~/.local/bin/litebase
export PATH="$HOME/.local/bin:$PATH"
```

Verify the installation by running:
```bash
litebase --help
```
