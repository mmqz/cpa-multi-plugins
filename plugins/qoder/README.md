# qoder

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) provider plugin for the Qoder family. The CN and Intl variants share one plugin and one identifier; accounts are routed by the region recorded in each auth file, and legacy per-region auth files are adopted on startup.

Built with Go (CGO, c-shared). Release artifacts for all supported platforms are produced by CI from tagged commits — see the repository root README for the version matrix and the disclaimer.

## License

MIT — see [LICENSE](LICENSE).

## Disclaimer

The code originates from the upstream reference repository and is provided for mirroring purposes only.
