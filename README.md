# aiproxy

A local proxy for AI coding agents.

It sits between your agent and one or more upstream provider accounts, rotates
between accounts as quota is consumed, and records every request so token usage
can be accounted for and graphed over time by model, account, and interval.

One binary. Run `aiproxy`, log in from the TUI, point your agent at it.

**Status: pre-implementation.** The design is settled and written up in
[docs/superpowers/specs/2026-08-17-aiproxy-design.md](docs/superpowers/specs/2026-08-17-aiproxy-design.md);
code is being built from it in the stages listed in that document.

## License

MIT
