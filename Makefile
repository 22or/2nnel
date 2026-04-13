BIN_SERVER := 2nnel-server
BIN_CLIENT := 2nnel
LDFLAGS    := -ldflags="-s -w"

.PHONY: all server client install clean

all: server client

server:
	go build $(LDFLAGS) -o $(BIN_SERVER) ./cmd/server/

client:
	go build $(LDFLAGS) -o $(BIN_CLIENT) ./cmd/client/

install: all
	install -m 755 $(BIN_SERVER) /usr/local/bin/$(BIN_SERVER)
	install -m 755 $(BIN_CLIENT) /usr/local/bin/$(BIN_CLIENT)

clean:
	rm -f $(BIN_SERVER) $(BIN_CLIENT)
