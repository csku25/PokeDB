all: server client

#for unoptimized: add '-gcflags="-N -l"' before -o
server: server_dir/server.go
	go build -o server server_dir/server.go

#for dlv with script: dlv -r script.name exec -- client -h localhost -p <port>
client: client_dir/client.go
	go build -o client client_dir/client.go
	
.PHONY: clean run
clean:
	rm -f server client

run_server: server poke.bin
	./server -p 12345 -m poke.bin -t trainers.bin -l server.log

run_client: client
	./client -h localhost -p 12345