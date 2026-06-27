.PHONY: build
build:
	go build -tags systemd -o ./bin/ ./...

.PHONY: install_local
install_local: build
	cp ./bin/serveroute ~/.local/bin
