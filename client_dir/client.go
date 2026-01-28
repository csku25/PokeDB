/*
Filename:  client.go
Description:
  - Client-side REPL for interacting with a pokemon database server via network sockets
  - Parses and validates user commands, sends formatted requests and handles JSON encoding
  - Manages bidirectional communication with the server via goroutines and channels
  - Deferred setup/teardown, gracefully exits when server shuts down
*/
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"

	"project3/recordlib"
	"golang.org/x/sys/unix"
)

//multiple error defs
var (
	ErrServer           = fmt.Errorf("error occurred on server-side")
	ErrInvalidReq       = fmt.Errorf("invalid request, check arguments")
	ErrGetNoArg         = fmt.Errorf("'get' requires at least 1 argument")
	ErrGetPokeNoID      = fmt.Errorf("'get pokemon' requires <id>: int")
	ErrGetPokeIDLess    = fmt.Errorf("pokemon id starts at 1")
	ErrGetPokeManyArg   = fmt.Errorf("'get pokemon' expects only 1 argument <id>: int")
	ErrPokeNotFound     = fmt.Errorf("pokemon ID not found")
	ErrGetTrainerArgs   = fmt.Errorf("'get trainer' expects 0 or 1 argument <id>: int")
	ErrGetTrainerIDLess = fmt.Errorf("trainer id starts at 1")
	ErrTrainerNotFound  = fmt.Errorf("trainer ID not found")
	ErrTrainerFileEmpty = fmt.Errorf("there are currently no trainers")
	ErrPostArgsMissing  = fmt.Errorf("'post' requires at least 3 arguments - trainer <name> <pokemon_id> [<pokemon_id> ...]")
	ErrPostPokeMax      = fmt.Errorf("'post' allows max. 6 pokemon")
	ErrPutArgsMissing   = fmt.Errorf("'put' requires at least 3 arguments - trainer <id> <pokemon_id> [<pokemon_id> ...]")
	ErrPutPokeMax       = fmt.Errorf("'put' allows max. 6 pokemon")
	ErrPostLongName     = fmt.Errorf("name too long, max 15 characters")
	ErrBadPost          = fmt.Errorf("one or more pokemon IDs were not found")
	ErrGetLogNoN        = fmt.Errorf("'get log' requires <n>: int")
	ErrGetLogManyArg    = fmt.Errorf("'get log' expects only 1 argument <id>: int")
)

/*
Function Name:  get_opts
Description:	parses flag arguments for client program
				exits if -help or --help used for help
Parameters:     N/A
Return Value:   the two required arguments and error (if any)
Type:           n/a -> string, int, error
*/
func get_opts() (string, int, error) {
	help_flag := flag.Bool("help", false, "Show help (must be used on its own)")
	host_flag := flag.String("h", "", "Server's host IP")
	port_flag := flag.Int("p", -1, "Port number")

	flag.Parse()
	if *help_flag {
		if flag.NFlag() > 1 {
			return "", -1, fmt.Errorf("-help must be used alone")
		}
		fmt.Println("Usage:")
		fmt.Println("  -h string\n        Server's host IP")
		fmt.Println("  -p int\n        Port number (10000-65535)")
		unix.Exit(0)
	}

	if *host_flag == "" || *port_flag == -1 {
		return "", -1, fmt.Errorf("-h and -p are required")
	}

	if *port_flag < 10000 || *port_flag > 65535 {
		fmt.Println("Error: Invalid port number!")
		fmt.Println("Must be in range 10000-65535")
		unix.Exit(1)
	}

	return *host_flag, *port_flag, nil
}

/*
Function Name:  server_resp
Description:	handles receiving server responses via resp_chan
				and server exit notification via server_exit chan
Parameters:		resp: channel to receive server responses
				server_exit: channel to notify client of server shutdown
Return Value:   server response string if received otherwise io.EOF
Type:           chan string, chan struct{} -> string, error
*/
func server_resp(resp chan string, server_exit chan struct{}) (string, error) {
	select {
	case msg := <-resp:
		return msg, nil
	case <-server_exit:
		return "", io.EOF
	}
}

/*
Function Name:  repl
Description:	handles one iteration of the REPL loop
				parses user input, validates commands/args
				sends formatted requests to server via client socket
				receives responses from server via resp_chan
Parameters:		sock: file stream to communicate with server
				scanner: used to read user input
				resp_chan: used to receive server responses
				server_exit: used to notify client of server shutdown
Return Value:   nil if all input and output is good otherwise error
Type:           *os.File, *bufio.Scanner, chan string, chan struct{} -> error
*/
func repl(sock *os.File, scanner *bufio.Scanner, resp_chan chan string, server_exit chan struct{}) error {
	fmt.Printf("PokeDB> ")

	if !scanner.Scan() {
		if scanner.Err() == nil { //CTRL-D
			fmt.Println()
			return io.EOF
		}
		return scanner.Err()
	}
	cmd := strings.Fields(strings.TrimSpace(scanner.Text()))
	cmd_len := len(cmd)
	if cmd_len == 0 {
		return nil
	}

	switch cmd[0] {
	case "":
		return nil

	case "exit":
		//indicate to server
		return io.EOF

	case "help":
		fmt.Println("Valid options:")
		fmt.Println("  exit")
		fmt.Println("  get pokemon <id>")
		fmt.Println("  get trainer")
		fmt.Println("  get trainer <id>")
		fmt.Println("  post trainer <name> <pokemon 1> [... <pokemon 6>]")
		fmt.Println("  put trainer <id> <pokemon 1> [... <pokemon 6>]")
		fmt.Println("  delete trainer <id>")
		fmt.Printf("  get log <n>\n\n")
		return nil

	case "get":
		if cmd_len >= 2 {
			switch cmd[1] {
			case "pokemon":
				if cmd_len < 3 {
					return ErrGetPokeNoID
				} else if cmd_len > 3 {
					return ErrGetPokeManyArg
				} else {
					num, err := strconv.Atoi(cmd[2])
					if err != nil {
						return err
					} else {
						if num <= 0 {
							return ErrGetPokeIDLess
						}
					}
					req := fmt.Sprintf("REQ_POKE_ID %s", cmd[2])
					recordlib.ReallyWrite(sock, req)

					bytes, err := server_resp(resp_chan, server_exit)
					if err != nil {
						fmt.Println("Warning: Server is shutting down.\nRequest not processed, exiting client...")
						return err
					}
					switch bytes {
					case "CLIENT_REQ_INVALID":
						return ErrInvalidReq
					case "SERVER_ERROR":
						return ErrServer
					case "OUT_OF_BOUNDS":
						return ErrPokeNotFound
					default:
						var pokemon recordlib.PokeRec
						if err := json.Unmarshal([]byte(bytes), &pokemon); err != nil {
							return err
						} else {
							pokemon.Print()
							return nil
						}
					}
				}

			case "trainer":
				switch cmd_len {
				case 3:
					num, err := strconv.Atoi(cmd[2])
					if err != nil {
						return err
					} else {
						if num <= 0 {
							return ErrGetTrainerIDLess
						}
					}
					req := fmt.Sprintf("REQ_TRAINER_ID %s", cmd[2])
					recordlib.ReallyWrite(sock, req)

					bytes, err := server_resp(resp_chan, server_exit)
					if err != nil {
						fmt.Println("Warning: Server is shutting down.\nRequest not processed, exiting client...")
						return err
					}
					switch bytes {
					case "CLIENT_REQ_INVALID":
						return ErrInvalidReq
					case "SERVER_ERROR":
						return ErrServer
					case "OUT_OF_BOUNDS":
						return ErrTrainerNotFound
					default:
						var trainer recordlib.TrainerRec
						if err := json.Unmarshal([]byte(bytes), &trainer); err != nil {
							return err
						} else {
							trainer.Print()
							return nil
						}
					}

				case 2:
					req := "REQ_TRAINER_ALL"
					recordlib.ReallyWrite(sock, req)

					ready, err := server_resp(resp_chan, server_exit)
					if err != nil {
						fmt.Println("Warning: Server is shutting down.\nRequest not processed, exiting client...")
						return err
					}
					switch ready {
					case "CLIENT_REQ_INVALID":
						return ErrInvalidReq
					case "SERVER_ERROR":
						return ErrServer
					case "OUT_OF_BOUNDS":
						return ErrTrainerFileEmpty
					case "FILE_ERROR":
						return fmt.Errorf("trainers file corrupted")
					case "SENDING":
						break
					}

					for {
						bytes, err := server_resp(resp_chan, server_exit)
						if err != nil {
							fmt.Println("Warning: Server is shutting down.\nRequest not processed, exiting client...")
							return err
						}
						switch bytes {
						case "SERVER_ERROR":
							return ErrServer
						case "OUT_OF_BOUNDS":
							return ErrTrainerFileEmpty
						case "DONE":
							return nil
						default:
							var trainer recordlib.TrainerRec
							if err := json.Unmarshal([]byte(bytes), &trainer); err != nil {
								return err
							} else {
								trainer.Print()
							}
						}
					}

				default:
					return ErrGetTrainerArgs
				}

			case "log":
				if cmd_len < 3 {
					return ErrGetLogNoN
				} else if cmd_len > 3 {
					return ErrGetLogManyArg
				}
				n, err := strconv.Atoi(cmd[2])
				if err != nil {
					return fmt.Errorf("invalid argument for 'get log'")
				} else if n <= 0 {
					return fmt.Errorf("argument <n> must be a positive integer")
				}

				req := fmt.Sprintf("REQ_LOG_FILE %s", cmd[2])
				recordlib.ReallyWrite(sock, req)
				bytes, err := server_resp(resp_chan, server_exit)
				if err != nil {
					fmt.Println("Warning: Server is shutting down.\nRequest not processed, exiting client...")
					return err
				}
				
				switch bytes {
				case "CLIENT_REQ_INVALID":
					return ErrInvalidReq
				case "SERVER_ERROR":
					return ErrServer
				default:
					fmt.Printf("\nRequested Log Entries\n")
					fmt.Println(bytes)
					fmt.Printf("End of Log\n\n")
					return nil
				}

			default:
				return fmt.Errorf("'%s' invalid option for get", cmd[1])
			}
		} else {
			return ErrGetNoArg
		}

	case "post":
		if cmd_len >= 4 {
			if cmd_len <= 9 {
				if cmd[1] != "trainer" {
					return fmt.Errorf("'%s' invalid option for post", cmd[1])
				}
				req := fmt.Sprintf("POST_TRAINER %s %s", cmd[2], cmd[3])
				for idx := 4; idx < cmd_len; idx++ {
					req += " " + cmd[idx]
				}
				recordlib.ReallyWrite(sock, req)

				bytes, err := server_resp(resp_chan, server_exit)
				if err != nil {
					fmt.Println("Warning: Server is shutting down.\nRequest not processed, exiting client...")
					return err
				}
				switch bytes {
				case "CLIENT_REQ_INVALID":
					return ErrInvalidReq
				case "SERVER_ERROR":
					return ErrServer
				case "LONG_NAME":
					return ErrPostLongName
				case "BAD_POST":
					return ErrBadPost
				default:
					fmt.Printf("Added Trainer '%s' to Trainer Database\n", cmd[2])
					fmt.Printf("New Trainer ID: %s\n\n", bytes)
					return nil
				}
			} else {
				return ErrPostPokeMax
			}
		} else {
			return ErrPostArgsMissing
		}

	case "put":
		if cmd_len >= 4 {
			if cmd_len <= 9 {
				if cmd[1] != "trainer" {
					return fmt.Errorf("'%s' invalid option for post", cmd[1])
				}
				req := fmt.Sprintf("PUT_TRAINER %s %s", cmd[2], cmd[3])
				for idx := 4; idx < cmd_len; idx++ {
					req += " " + cmd[idx]
				}
				recordlib.ReallyWrite(sock, req)

				bytes, err := server_resp(resp_chan, server_exit)
				if err != nil {
					fmt.Println("Warning: Server is shutting down.\nRequest not processed, exiting client...")
					return err
				}
				opt_bytes := strings.Split(bytes, ".")
				switch opt_bytes[0] {
				case "CLIENT_REQ_INVALID":
					return ErrInvalidReq
				case "SERVER_ERROR":
					return ErrServer
				case "BAD_PUT":
					return fmt.Errorf("%s", opt_bytes[1])
				case "GOOD_PUT":
					fmt.Printf("Updated Trainer ID: %s\n\n", cmd[2])
					return nil
				default:
					return fmt.Errorf("put: extraneous error")
				}
			} else {
				return ErrPutPokeMax
			}
		} else {
			return ErrPutArgsMissing
		}

	case "delete":
		if cmd_len == 3 {
			if cmd[1] != "trainer" {
				return fmt.Errorf("'%s' invalid option for delete", cmd[1])
			}
			req := fmt.Sprintf("DEL_TRAINER %s", cmd[2])
			recordlib.ReallyWrite(sock, req)

			bytes, err := server_resp(resp_chan, server_exit)
			if err != nil {
				fmt.Println("Warning: Server is shutting down.\nRequest not processed, exiting client...")
				return err
			}
			switch bytes { //server error not possible?
			case "CLIENT_REQ_INVALID":
				return ErrInvalidReq
			case "OUT_OF_BOUNDS":
				return ErrTrainerNotFound
			case "DELETED":
				fmt.Printf("Deleted Trainer ID: %s\n\n", cmd[2])
				return nil
			default:
				return fmt.Errorf("delete: extraneous error")
			}

		} else {
			return fmt.Errorf("'delete' requires at 2 arguments - trainer <id>: int")
		}

	default:
		return fmt.Errorf("'%s' invalid command", cmd[0])
	}
}

func main() {
	host, port, err := get_opts()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		fmt.Printf("Usage:\n")
		fmt.Println(" --help\n       Show help (must be used on its own)")
		fmt.Println("  -h string\n        Server's host IP")
		fmt.Println("  -p int\n        Port number (10000-65535)")
		unix.Exit(1)
	}

	sock_fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		log.Printf("Error: Failed to create socket!\n%v", err)
		unix.Exit(1)
	}

	var host_addr [4]byte
	if host == "localhost" {
		host_addr = [4]byte{127, 0, 0, 1}
	} else {
		parsed_ip := net.ParseIP(host).To4()
		host_addr = [4]byte(parsed_ip.To4())
	}

	addr := &unix.SockaddrInet4{Addr: host_addr, Port: port}
	if err := unix.Connect(sock_fd, addr); err != nil { //handles timeout
		log.Printf("Error: Failed to connect to server!\n%v", err)
		unix.Exit(1)
	}
	sock := os.NewFile(uintptr(sock_fd), "socket")
	if sock == nil {
		log.Println("Error: Failed to create socket stream!")
		unix.Exit(1)
	}
	defer func() {
		if err := sock.Close(); err != nil {
			log.Printf("Error: Failed to close socket!\n%v", err)
		}
	}() //sock_fd closed on sock.Close()

	e_port, err := recordlib.ReallyRead(sock)
	if err != nil {
		fmt.Println("Error: Failed to read ephemeral port from server!")
		return
	}
	fmt.Printf("Pokemon DataBase REPL\nConnected to localhost | ephemeral port %s\n", e_port)
	scanner := bufio.NewScanner(os.Stdin)
	response := make(chan string)
	server_exit := make(chan struct{})

	go func() {
		for {
			serv_msg, err := recordlib.ReallyRead(sock)
			if err != nil {
				log.Printf("Error reading from server: %v", err)
				recordlib.ReallyWrite(sock, "EXIT")
				close(server_exit)
				return
			}

			serv_msg = strings.TrimSpace(serv_msg)
			if serv_msg == "BYE" {
				recordlib.ReallyWrite(sock, "EXIT")
				close(server_exit)
				return
			}

			response <- serv_msg
		}
	}()

	for {
		select {
		case <-server_exit:
			fmt.Println("Warning: Server is shutting down.\nChanges saved, exiting client...")
			return //notified in REPL

		default:
			err := repl(sock, scanner, response, server_exit)
			if err != nil {
				if err == io.EOF {
					recordlib.ReallyWrite(sock, "EXIT")
					return
				}
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				if err != ErrServer {
					fmt.Fprintf(os.Stderr, "For valid options, type 'help'\n\n")
				} else {
					fmt.Println()
				}
			}
		}
	}
}
