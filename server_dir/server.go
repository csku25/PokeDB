/*
Filename:  server.go
Description:
  - Pokemon database server listens for client connections via network sockets
  - Handles requests to read pokemon records and CRUD with trainer records and server log
  - Processes client commands using regex patterns, performs file-based operations and responding with JSON or status codes
  - Concurrent handling of clients, error logging and recovery from potential panics
  - Deferred setup/teardown, gracefully exits upon receiving interrupt signal
*/
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"unsafe"

	"project3/recordlib"
	"golang.org/x/sys/unix"
)

/*
Function Name:  get_opts
Description:    parses flag arguments for server program
				exits if -h for help
Parameters:     N/A
Return Value:   the three required arguments and error (if any)
Type:           n/a -> int, string, string, error
*/
func get_opts() (int, string, string, string, error) {
	help_flag := flag.Bool("h", false, "Show help (must be used on its own)")
	port_flag := flag.Int("p", -1, "Port number")
	bin_file_flag := flag.String("m", "", "Name of Pokemon binary file")
	trainer_file_flag := flag.String("t", "", "Name of trainer binary file")
	log_file_flag := flag.String("l", "", "Name of log file")

	flag.Parse()
	if *help_flag {
		if flag.NFlag() > 1 {
			return -1, "", "", "", fmt.Errorf("-h must be used alone")
		}
		fmt.Println("Usage:")
		flag.PrintDefaults()
		unix.Exit(0)
	}

	if *port_flag == -1 || *bin_file_flag == "" || *trainer_file_flag == "" || *log_file_flag == "" {
		return -1, "", "", "", fmt.Errorf("-p, -m, -t, and -l are required")
	}

	return *port_flag, *bin_file_flag, *trainer_file_flag, *log_file_flag, nil
}

/*
Function Name:  process_req_get_poke
Description:    parses GET pokemon requests, reads pokemon record from
				pokemon file under read lock, send JSON or status to client
Parameters:     req: raw client request
                client: client socket file for reply
                src_port: client source port (for logging)
                poke_file: pokemon binary file
                poke_lock: RW lock protecting poke_file
Return Value:   n/a
Type:           string, *os.File, int, *os.File, *sync.RWMutex -> n/a
*/
func process_req_get_poke(req string, client *os.File, src_port int, poke_file *os.File, poke_lock *sync.RWMutex) {
	captures := recordlib.ReqGetPokeID.FindStringSubmatch(req)
	log.Printf("[127.0.0.1:%d] %s\n", src_port, req)
	if len(captures) > 0 {
		id, _ := strconv.Atoi(captures[1])
		//can't trust that regexp is 100% perfect
		//but assuming Atoi should not fail on matched regexp
		poke_lock.RLock()
		rec, err := recordlib.GetPokemon(poke_file, uint16(id))
		poke_lock.RUnlock()

		if err != nil {
			if err == io.EOF {
				fmt.Printf("[%d] Client requested id out of bounds\n", src_port)
				recordlib.ReallyWrite(client, "OUT_OF_BOUNDS")
			} else {
				fmt.Printf("[%d] Error in GetPokemon: %v\n", src_port, err)
				recordlib.ReallyWrite(client, "SERVER_ERROR")
			}
		} else {
			bytes, err := json.Marshal(rec)
			if err != nil {
				fmt.Printf("[%d] Error on json encoding: %v\n", src_port, err)
				recordlib.ReallyWrite(client, "SERVER_ERROR")
			} else {
				recordlib.ReallyWrite(client, string(bytes))
				fmt.Printf("[%d] Pokemon record sent to client\n", src_port)
			}
		}
	}
}

/*
Function Name:  process_req_get_trainer
Description:    parses GET trainer requests, reads trainer record using
                global manager record-level locks, sends JSON or status
Parameters:     req: raw client request
                client: client socket file for reply
                src_port: client source port (for logging)
                trainer_file: trainer binary file
                gm: record-level lock manager
Return Value:   n/a
Type:           string, *os.File, int, *os.File, *recordlib.GlobalManager -> n/a
*/
func process_req_get_trainer(req string, client *os.File, src_port int, trainer_file *os.File, gm *recordlib.GlobalManager) {
	captures := recordlib.ReqGetTrainerID.FindStringSubmatch(req)
	log.Printf("[127.0.0.1:%d] %s\n", src_port, req)
	if len(captures) > 0 {
		id, _ := strconv.Atoi(captures[1])
		gm.RLockRecord(uint16(id))
		rec, err := recordlib.GetTrainer(trainer_file, uint16(id))
		gm.RUnlockRecord(uint16(id))

		if err != nil {
			if err == io.EOF || err.Error() == "trainer ID not found" {
				fmt.Printf("[%d] Client requested id out of bounds\n", src_port)
				recordlib.ReallyWrite(client, "OUT_OF_BOUNDS")
			} else {
				fmt.Printf("[%d] Error in GetTrainer: %v\n", src_port, err)
				recordlib.ReallyWrite(client, "SERVER_ERROR")
			}
		} else {
			bytes, err := json.Marshal(rec)
			if err != nil {
				fmt.Printf("[%d] Error on json encoding: %v\n", src_port, err)
				recordlib.ReallyWrite(client, "SERVER_ERROR")
			} else {
				recordlib.ReallyWrite(client, string(bytes))
				fmt.Printf("[%d] Trainer record sent to client\n", src_port)
			}
		}
	}
}

/*
Function Name:  process_req_get_trainer_all
Description:    handle request to stream all trainer records, acquires
                read-all lock from global manager, validates file size and
                iterates records, sending JSON lines or status
Parameters:     req: raw client request
                client: client socket file for reply
                src_port: client source port (for logging)
                trainer_file: trainer binary file
                gm: record-level lock manager
Return Value:   n/a
Type:           string, *os.File, int, *os.File, *recordlib.GlobalManager -> n/a
*/
func process_req_get_trainer_all(req string, client *os.File, src_port int, trainer_file *os.File, gm *recordlib.GlobalManager) {
	log.Printf("[127.0.0.1:%d] %s\n", src_port, req)
	gm.LockReadAll()
	trainer_size := int64(unsafe.Sizeof(recordlib.TrainerRec{}))
	info, err := trainer_file.Stat()
	if err != nil {
		fmt.Printf("[%d] Error in file.Stat: %v\n", src_port, err)
		recordlib.ReallyWrite(client, "SERVER_ERROR")
		gm.UnlockReadAll()
		return
	}
	file_size := info.Size()
	if file_size == 0 {
		fmt.Printf("[%d] Client requested from empty file\n", src_port)
		recordlib.ReallyWrite(client, "OUT_OF_BOUNDS")
		gm.UnlockReadAll()
		return
	}
	if file_size%trainer_size != 0 { //gofmt pushes these together?
		fmt.Printf("[%d] Error: file size is not a multiple of record size\n", src_port)
		recordlib.ReallyWrite(client, "FILE_ERROR")
		gm.UnlockReadAll()
		return
	}
	count := 0
	idx := 1

	recordlib.ReallyWrite(client, "SENDING")
	for {
		trainer, err := recordlib.GetTrainer(trainer_file, uint16(idx))
		if err != nil {
			if err.Error() == "trainer ID not found" {
				idx++
				continue //blank record from deletion
			}
			break //EOF
		}
		bytes, err := json.Marshal(trainer)
		if err != nil {
			fmt.Printf("[%d] Error in json encoding: %v\n", src_port, err)
			recordlib.ReallyWrite(client, "SERVER_ERROR")
			break
		} else {
			recordlib.ReallyWrite(client, string(bytes))
			idx++
			count++
		}
	}

	gm.UnlockReadAll()
	if count == 0 {
		fmt.Printf("[%d] Client requested from empty file\n", src_port)
		recordlib.ReallyWrite(client, "OUT_OF_BOUNDS")
	} else {
		recordlib.ReallyWrite(client, "DONE")
		fmt.Printf("[%d] All Trainer records sent to client\n", src_port)
	}
}

/*
Function Name:  process_req_post_trainer
Description:    parses a POST trainer request, validates name and pokemon IDs,
                acquires global poke read lock and trainer write locking to
                append, reply with id or status
Parameters:     req: raw client request
                client: client socket file for reply
                src_port: client source port (for logging)
                poke_file: pokemon binary file
                trainer_file: trainer binary file
                poke_lock: RW lock protecting poke_file
                gm: record-level lock manager
Return Value:   n/a
Type:           string, *os.File, int, *os.File, *os.File, *sync.RWMutex, *recordlib.GlobalManager -> n/a
*/
func process_req_post_trainer(req string, client *os.File, src_port int, poke_file *os.File, trainer_file *os.File, poke_lock *sync.RWMutex, gm *recordlib.GlobalManager) {
	captures := recordlib.ReqPostTrainer.FindStringSubmatch(req)
	log.Printf("[127.0.0.1:%d] %s", src_port, req)
	if len(captures) > 0 {
		var name string
		var pokemon []uint16
		for idx := 1; idx < len(captures); idx++ {
			if idx == 1 {
				name = captures[1]
				if len(name) > 15 {
					fmt.Printf("[%d] Refuse to post: name too long\n", src_port)
					recordlib.ReallyWrite(client, "LONG_NAME")
					break
				}
			} else {
				if captures[idx] == "" {
					break
				}
				num, err := strconv.Atoi(captures[idx])
				if err != nil {
					fmt.Printf("[%d] Error: %v\n", src_port, err)
					recordlib.ReallyWrite(client, "SERVER_ERROR")
					return
				}
				pokemon = append(pokemon, uint16(num))
			}
		}
		if len(pokemon) == 0 {
			return
		}
		gm.GlobalLock.RLock()
		poke_lock.Lock()
		id, err := recordlib.PostTrainer(trainer_file, poke_file, name, pokemon)
		poke_lock.Unlock()
		gm.GlobalLock.RUnlock()

		if err != nil {
			fmt.Printf("[%d] Error in PostTrainer: %v", src_port, err)
			recordlib.ReallyWrite(client, "BAD_POST")
		} else if id != 0 {
			recordlib.ReallyWrite(client, strconv.Itoa(int(id)))
			fmt.Printf("[%d] Post successful, trainer file modified, id sent to client", src_port)
		}
	}
}

/*
Function Name:  process_req_put_trainer
Description:    parses a PUT trainer request, trainer ID,
                acquires global poke read lock and trainer write locking to
                append, reply with id or status
Parameters:     req: raw client request
                client: client socket file for reply
                src_port: client source port (for logging)
                poke_file: pokemon binary file
                trainer_file: trainer binary file
                poke_lock: RW lock protecting poke_file
                gm: record-level lock manager
Return Value:   n/a
Type:           string, *os.File, int, *os.File, *os.File, *sync.RWMutex, *recordlib.GlobalManager -> n/a
*/
func process_req_put_trainer(req string, client *os.File, src_port int, poke_file *os.File, trainer_file *os.File, poke_lock *sync.RWMutex, gm *recordlib.GlobalManager) {
	captures := recordlib.ReqPutTrainer.FindStringSubmatch(req)
	log.Printf("[127.0.0.1:%d] %s\n", src_port, req)
	if len(captures) > 0 {
		var id uint16
		var pokemon []uint16
		for idx := 1; idx < len(captures); idx++ {
			if idx == 1 {
				num, err := strconv.Atoi(captures[idx])
				if err == nil {
					id = uint16(num)
				} else {
					fmt.Printf("[%d] Error: %v\n", src_port, err)
					recordlib.ReallyWrite(client, "SERVER_ERROR")
					return
				}
			} else {
				if captures[idx] == "" {
					break
				}
				num, err := strconv.Atoi(captures[idx])
				if err != nil {
					fmt.Printf("[%d] Error in Atoi: %v\n", src_port, err)
					recordlib.ReallyWrite(client, "SERVER_ERROR")
					return
				}
				pokemon = append(pokemon, uint16(num))
			}
		}
		gm.WLockRecord(id)
		poke_lock.Lock()
		err := recordlib.PutTrainer(trainer_file, poke_file, id, pokemon)
		poke_lock.Unlock()
		gm.WUnlockRecord(id)

		if err != nil {
			fmt.Printf("[%d] Error in PutTrainer: %v\n", src_port, err)
			err_msg := fmt.Sprintf("BAD_PUT.%s", err)
			recordlib.ReallyWrite(client, err_msg)
		} else if id != 0 {
			recordlib.ReallyWrite(client, "GOOD_PUT")
			fmt.Printf("[%d] Put successful, trainer file modified", src_port)
		}
	}
}

/*
Function Name:  process_req_delete_trainer
Description:    parses a DELETE trainer request, lock the specific trainer record
                using global manager, perform logical deletion, reply with result
Parameters:     req: raw client request
                client: client socket file for reply
                src_port: client source port (for logging)
                trainer_file: trainer binary file
                gm: record-level lock manager
Return Value:   n/a
Type:           string, *os.File, int, *os.File, *recordlib.GlobalManager -> n/a
*/
func process_req_delete_trainer(req string, client *os.File, src_port int, trainer_file *os.File, gm *recordlib.GlobalManager) {
	captures := recordlib.ReqDelTrainer.FindStringSubmatch(req)
	log.Printf("[127.0.0.1:%d] %s\n", src_port, req)
	if len(captures) > 0 {
		id, _ := strconv.Atoi(captures[1])
		gm.WLockRecord(uint16(id))
		if err := recordlib.DeleteTrainer(trainer_file, uint16(id)); err != nil {
			fmt.Printf("[%d] Error in DeleteTrainer: %v\n", src_port, err)
			recordlib.ReallyWrite(client, "OUT_OF_BOUNDS")
		} else {
			recordlib.ReallyWrite(client, "DELETED")
			fmt.Printf("[%d] Logically deleted record, trainer file modified\n", src_port)
		}
		gm.WUnlockRecord(uint16(id))
	}
}

/*
Function Name:  process_req_get_log
Description:    parses a GET log N request, read last N log entries
                sends back logs or error status
Parameters:     req: raw client request
                client: client socket file for reply
                src_port: client source port (for logging)
                log_file: server log file
                log_lock: mutex protecting log_file
Return Value:   n/a
Type:           string, *os.File, int, *os.File, *sync.Mutex -> n/a
*/
func process_req_get_log(req string, client *os.File, src_port int, log_file *os.File, log_lock *sync.Mutex) {
	captures := recordlib.ReqGetLogN.FindStringSubmatch(req)
	log.Printf("[127.0.0.1:%d] %s\n", src_port, req)
	if len(captures) > 0 {
		n, _ := strconv.Atoi(captures[1])
		log_lock.Lock()
		logs, err := recordlib.LogReadN(log_file, n)
		if err != nil {
			fmt.Printf("[%d] Error in GetLog: %v\n", src_port, err)
			recordlib.ReallyWrite(client, "SERVER_ERROR")
		} else {
			recordlib.ReallyWrite(client, logs)
			fmt.Printf("[%d] Requested logs sent to client\n", src_port)
		}
		log_lock.Unlock()
	}
}

/*
Function Name:  handle_client
Description:	handles client requests, concurrent handling of clients
				error logging and recovery from potential panics
Parameters:		src_port: source port of client connection
				client: client's socket file stream
				poke_file: file to read pokemon records from
				trainer_file: file to read trainer records from
				log_file: file to write logs to and read from
				poke_lock: mutex lock for pokemon file access
				gm: global manager for mutex locks for trainer file access
				log_lock: mutex lock for log file access
				client_exit: channel to send to client to exit
Return Value:   n/a
Type:           int, *os.File, *os.File, *os.File, *os.File, *sync.RWMutex, *recordlib.GlobalManager, *sync.Mutex, chan<- *os.File -> n/a
*/
func handle_client(src_port int, client *os.File, poke_file *os.File, trainer_file *os.File, log_file *os.File, poke_lock *sync.RWMutex, gm *recordlib.GlobalManager, log_lock *sync.Mutex, client_exit chan<- *os.File) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[%d] Recovered from panic in client handler: %v", src_port, r)
		}
		client_exit <- client
	}()

	recordlib.ReallyWrite(client, strconv.Itoa(src_port))
	for {
		req, err := recordlib.ReallyRead(client)
		if err != nil {
			if err == io.EOF {
				log.Printf("[127.0.0.1:%d] Client disconnected (EOF).\n", src_port)
				return
			}
			fmt.Printf("[%d] Error on read: %v\n", src_port, err)
		}

		switch {
		case req == "EXIT":
			fmt.Printf("\r")
			log.Printf("[127.0.0.1:%d] Client disconnected.\n", src_port)
			client_exit <- client
			return

		case recordlib.ReqGetPokeID.MatchString(req): //get pokemon _
			process_req_get_poke(req, client, src_port, poke_file, poke_lock)

		case recordlib.ReqGetTrainerID.MatchString(req): //get trainer _
			process_req_get_trainer(req, client, src_port, trainer_file, gm)

		case recordlib.ReqGetTrainerAll.MatchString(req): //get trainer
			process_req_get_trainer_all(req, client, src_port, trainer_file, gm)

		case recordlib.ReqPostTrainer.MatchString(req): //post trainer _ _ ...
			process_req_post_trainer(req, client, src_port, poke_file, trainer_file, poke_lock, gm)

		case recordlib.ReqPutTrainer.MatchString(req): //put trainer _ _ ...
			process_req_put_trainer(req, client, src_port, poke_file, trainer_file, poke_lock, gm)

		case recordlib.ReqDelTrainer.MatchString(req): //delete trainer _
			process_req_delete_trainer(req, client, src_port, trainer_file, gm)

		case recordlib.ReqGetLogN.MatchString(req):
			process_req_get_log(req, client, src_port, log_file, log_lock)

		default:
			log.Printf("[127.0.0.1:%d] Request didn't match valid options\n", src_port) //regexp didn't match, invalid arg from client
			recordlib.ReallyWrite(client, "CLIENT_REQ_INVALID")
		}
	}
}

func main() {
	port, poke_file_name, trainer_file_name, log_file_name, err := get_opts()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		fmt.Printf("Usage:\n")
		flag.PrintDefaults()
		unix.Exit(1) //doesn't have to return, no deferred cleanup
	}
	if port < 10000 || port > 65535 {
		fmt.Printf("Error: Invalid port number!\n")
		unix.Exit(1)
	}

	//set up and open the binary data files
	poke_fd, err := unix.Open(poke_file_name, unix.O_RDONLY, 0644)
	if err != nil {
		log.Fatalf("Error: Failed to open pokemon bin file!\n%v", err)
	}
	poke_file := os.NewFile(uintptr(poke_fd), poke_file_name)
	if poke_file == nil {
		if err := unix.Close(poke_fd); err != nil {
			log.Printf("Error: Failed to close poke_fd!\n%v", err)
		}
		log.Println("Error: Failed to wrap poke_fd into File!")
		return
	}
	defer func() {
		if err := poke_file.Close(); err != nil {
			log.Printf("Error: Failed to close poke bin file!\n%v", err)
		} //poke_fd closed on poke_file.Close()
	}()

	trainer_fd, err := unix.Open(trainer_file_name, unix.O_RDWR|unix.O_CREAT, 0644)
	if err != nil {
		log.Printf("Error: Failed to open trainer bin file!\n%v", err)
		return
	}
	trainer_file := os.NewFile(uintptr(trainer_fd), trainer_file_name)
	if trainer_file == nil {
		if err := unix.Close(trainer_fd); err != nil {
			log.Printf("Error: Failed to close trainer_fd!\n%v", err)
		}
		log.Println("Error: Failed to wrap trainer_fd into File!")
		return
	}
	defer func() {
		if err := trainer_file.Close(); err != nil {
			log.Printf("Error: Failed to close trainer bin file!\n%v", err)
		} //trainer_fd closed on trainer_file.Close()
	}()

	log_fd, err := unix.Open(log_file_name, unix.O_APPEND|unix.O_RDWR|unix.O_CREAT, 0644)
	if err != nil {
		log.Printf("Error: Failed to open log file!\n%v", err)
		return
	}
	log_file := os.NewFile(uintptr(log_fd), log_file_name)
	if log_file == nil {
		if err := unix.Close(log_fd); err != nil {
			log.Printf("Error: Failed to close log_fd!\n%v", err)
		}
		log.Println("Error: Failed to wrap log_fd into File!")
		return
	}
	defer func() {
		if err := log_file.Close(); err != nil {
			log.Printf("Error: Failed to close log file!\n%v", err)
		} //log_fd closed on log_file.Close()
	}()

	mw := io.MultiWriter(os.Stdout, log_file)
	log.SetOutput(mw)
	var poke_lock sync.RWMutex
	gm := recordlib.NewGlobalManager()
	var log_lock sync.Mutex //log always written to then read

	//use socket, serve on localhost:port
	sock_fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		fmt.Printf("Error: Failed to create socket!\n%v", err)
		return
	}
	defer func() {
		if err := unix.Close(sock_fd); err != nil {
			fmt.Printf("Error: Failed to close server socket!\n%v", err)
		}
	}()

	host := [4]byte{127, 0, 0, 1} //localhost
	addr := &unix.SockaddrInet4{Addr: host, Port: port}
	if err := unix.Bind(sock_fd, addr); err != nil {
		fmt.Printf("Error: Failed to bind socket!\n%v", err)
		return
	}
	if err := unix.Listen(sock_fd, 10); err != nil {
		fmt.Printf("Error: Failed to listen on socket!\n%v", err)
	}

	fmt.Printf("Listening on host - ")
	for _, b := range host {
		if b == host[3] {
			fmt.Printf("%d", b)
		} else {
			fmt.Printf("%d.", b)
		}
	}
	fmt.Printf(":%d\n", port)

	signal_chan := make(chan os.Signal, 1)
	signal.Notify(signal_chan, unix.SIGINT)

	new_client := make(chan *os.File)
	client_done := make(chan *os.File)
	accept_done := make(chan struct{})

	go func() {
		clients := make(map[*os.File]bool)
		shutting_down := false

		for {
			select {
			case client := <-new_client:
				if shutting_down {
					client.Close() //reject new clients during shutdown
				} else {
					clients[client] = true
				}

			case client := <-client_done:
				client.Close()
				delete(clients, client)

			case <-signal_chan:
				fmt.Printf("\r")
				log.Println("Interrupt received, shutting down server...")
				shutting_down = true
				conns := len(clients)

				if conns != 0 {
					for client := range clients {
						recordlib.ReallyWrite(client, "BYE")
						if <-client_done != nil {
							delete(clients, client)
						}
					}
					fmt.Println("All clients disconnected.")
				}
				close(accept_done)
				return
			}
		}
	}()

	go func() {
		for {
			client_fd, client_addr, err := unix.Accept(sock_fd)
			if err != nil {
				fmt.Printf("Server stopped accepting: %v", err)
				return
			}

			client_port := client_addr.(*unix.SockaddrInet4).Port
			log.Printf("Client connected - 127.0.0.1:%v\n", client_port)
			client_sock := os.NewFile(uintptr(client_fd), "client_sock")
			if client_sock == nil {
				continue
			}

			new_client <- client_sock
			go handle_client(client_port, client_sock, poke_file, trainer_file, log_file, &poke_lock, gm, &log_lock, client_done)
		}
	}()

	//wait for ALL clients
	<-accept_done
	signal.Stop(signal_chan)
}
