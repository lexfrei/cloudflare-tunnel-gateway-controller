# Contributing

Thank you for your interest in contributing to the Cloudflare Tunnel Gateway
Controller!

## Code of Conduct

Be respectful and constructive in all interactions. We welcome contributors
of all experience levels.

## How to Contribute

### Reporting Issues

- Search existing issues before creating a new one
- Use the issue templates for bug reports and feature requests
- Provide as much detail as possible (versions, logs, reproduction steps)

### Submitting Pull Requests

1. Fork the repository
2. Create a feature branch from `master`
3. Make your changes
4. Ensure tests pass and linting is clean
5. Submit a pull request

## Development Setup

See [Setup](setup.md) for detailed instructions.

Quick start:

```bash
git clone https://github.com/lexfrei/cloudflare-tunnel-gateway-controller.git
cd cloudflare-tunnel-gateway-controller
go mod download
go build ./...
golangci-lint run
```

## Commit Message Format

We use [Conventional Commits](https://www.conventionalcommits.org/):

```text
type(scope): brief description

Optional longer explanation.

Co-Authored-By: Your Name <your@email.com>
```

### Types

| Type | Description |
|------|-------------|
| `feat` | New feature |
| `fix` | Bug fix |
| `docs` | Documentation changes |
| `style` | Code style changes (formatting) |
| `refactor` | Code refactoring |
| `test` | Adding or updating tests |
| `chore` | Maintenance tasks |
| `ci` | CI/CD changes |
| `perf` | Performance improvements |
| `build` | Build system changes |

### Examples

```text
feat(controller): add support for GRPCRoute

fix(ingress): handle empty hostnames correctly

docs(readme): add installation instructions

chore(deps): update controller-runtime to v0.22.4
```

## Pull Request Process

1. **Title**: Use conventional commit format
2. **Description**: Fill out the PR template completely
3. **Tests**: Add tests for new functionality
4. **Linting**: Ensure `golangci-lint run` passes with no errors
5. **Documentation**: Update relevant docs
6. **Review**: Address reviewer feedback

### Testing PR Changes

Each PR automatically builds test artifacts. Links to test container images
and Helm chart are posted as a comment on the PR, allowing you to test
changes before merging.

### PR Checklist

- [ ] Code follows project style guidelines
- [ ] Tests added/updated for changes
- [ ] Documentation updated
- [ ] Commit messages follow conventional format
- [ ] All CI checks pass
- [ ] PR description is complete

## Code Style

### Go Code

- Follow standard Go conventions
- Use `golangci-lint` for linting (config in `.golangci.yaml`)
- Maximum function length: 60 lines
- Maximum cyclomatic complexity: 15
- Add godoc comments to exported types and functions

### Linting

All linting errors must be fixed before merging:

```bash
# Run linter
golangci-lint run

# Auto-fix some issues
golangci-lint run --fix
```

### nolint Directives

If you must disable a linter, always provide an explanation:

```go
//nolint:funlen // controller setup requires multiple initialization steps
func Run(ctx context.Context, cfg *Config) error {
```

## Testing

### Running Tests

```bash
# All tests
go test -v ./...

# With race detector
go test -race ./...

# With coverage
go test -coverprofile=coverage.out ./...
```

### Helm Chart Tests

```bash
helm unittest charts/cloudflare-tunnel-gateway-controller
```

### Test Requirements

- Unit tests for new functionality
- Table-driven tests preferred
- Mock external dependencies
- Test error cases

## Documentation

### What to Document

- New features in README.md
- API changes in Gateway API docs
- Configuration options
- Breaking changes

### Documentation Style

- Use clear, concise language
- Include code examples
- Keep formatting consistent with existing docs

## Release Process

Releases are automated via GitHub Actions:

1. Version is determined by commit messages (semantic versioning)
2. Container images are built and pushed to GHCR
3. Helm chart is published to OCI registry
4. GitHub Release is created with changelog

## Getting Help

- Open an issue for questions
- Check existing documentation in `docs/`
- Review closed issues for similar problems

## License

By contributing, you agree that your contributions will be licensed under
the BSD 3-Clause License.
