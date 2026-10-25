# qoder

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) provider plugin for the Qoder family. The CN and Intl variants share one plugin and one identifier; accounts are routed by the region recorded in each auth file, and legacy per-region auth files are adopted on startup.

Built with Go (CGO, c-shared). Release artifacts for all supported platforms are produced by CI from tagged commits — see the repository root README for the version matrix and the disclaimer.

## Queue wait budget

Set `plugins.configs.qoder.queue_wait_budget` to limit the cumulative time spent waiting on upstream `10605` / `RESOURCE_LIMIT` queue tickets:

```yaml
plugins:
  configs:
    qoder:
      enabled: true
      queue_wait_budget: 30
```

The value is an integer from 0 to 150 seconds. Omitting it preserves the existing 150-second budget; `0` disables queue retries. The existing maximum of three retries and 45-second per-wait cap still apply. With 30-second tickets, a budget of 30 permits one retry, while a 45-second ticket cannot fit and fails immediately.

This limits queue-ticket waits per plugin execution, not network or generation time or retries performed by the host. A shorter budget returns an explicit queue error sooner but may give up on requests that would succeed after a longer wait.

## License

MIT — see [LICENSE](LICENSE).

## Disclaimer

The code originates from the upstream reference repository and is provided for mirroring purposes only.
