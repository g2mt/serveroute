build:
  go build -o ./bin/ ./...

install_local: build
  cp ./bin/serveroute ~/.local/bin
