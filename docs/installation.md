# Installation

MailFerry is a single self-contained file. No pip, no virtualenv, no
dependencies — just Python 3.9+ (3.12+ recommended) on macOS or Linux.

## Standalone (recommended)

```bash
curl -LO https://github.com/ajsap/mailferry/releases/latest/download/mailferry.pyz
chmod +x mailferry.pyz
./mailferry.pyz --version
```

## From source

```bash
git clone https://github.com/ajsap/mailferry.git
cd mailferry
python3 -m mailferry --version
```

## From the wheel

```bash
pip install mailferry-<version>-py3-none-any.whl
mailferry --version
```

## Verifying a download

Each release ships a `SHA256SUMS` file:

```bash
curl -LO https://github.com/ajsap/mailferry/releases/latest/download/SHA256SUMS
sha256sum -c SHA256SUMS 2>/dev/null | grep mailferry.pyz
```

## Self-test

Confirm your environment is ready:

```bash
mailferry doctor
```

This checks the Python version, terminal/TTY capabilities, UTF-8 locale,
State Database writability, and the TLS certificate store.
