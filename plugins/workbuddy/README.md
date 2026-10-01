# workbuddy

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) provider plugin for the CodeBuddy / WorkBuddy family. The CN and Global/Intl variants share one plugin and one identifier; accounts are routed by the platform and region recorded in each auth file, and legacy per-platform auth files are adopted on startup.

Built with Go (CGO, c-shared). Release artifacts for all supported platforms are produced by CI from tagged commits — see the repository root README for the version matrix and the disclaimer.

## License

MIT — see [LICENSE](LICENSE).

## Disclaimer

The code originates from the upstream reference repository and is provided for mirroring purposes only.
