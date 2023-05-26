generate:
	go generate ./...

build: generate
	go build -o=./release/loggatherer.exe ./cmd
