# Development

See [AGENTS.md](AGENTS.md) for scope and data boundaries.

```sh
npm install --prefix web
npm run build --prefix web
go test ./...
go test -race ./...
go test ./agent/codexdesktop -run TestCUJ
go vet ./...
go build -o Brodge ./cmd/brodge
```

Do not commit local credentials or runtime databases. Real IM testing must use an explicitly authorized test conversation.
