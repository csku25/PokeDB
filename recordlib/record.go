/*
Filename:  record.go
Description:
  - Library implements binary record management for pokemon and trainers
  - Provides CRUD ops for trainer records, linking trainers to pokemon
  - Defines multiple mutual exclusion structures and functions to implement row-level concurrency
  - Helper functions format record printing and reliable file I/O operations
*/
package recordlib

import (
	"bytes"
	"container/list"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

type RecordLock struct {
	Lock *sync.Mutex
	Cond *sync.Cond
	NumReading int
	NumWriting int
	WrQueue *list.List
}

/*
Function Name:  NewRecordLock
Description:    Allocate and initialize RecordLock struct containing
                mutex, condition variable and writer queue
Parameters:     N/A
Return Value:   newly allocated and initialized RecordLock
Type:           n/a -> *RecordLock
*/
func NewRecordLock() *RecordLock {
	rec_lock := &RecordLock{
        Lock:    new(sync.Mutex),
        WrQueue: list.New(),
    }
    rec_lock.Cond = sync.NewCond(rec_lock.Lock)
	return rec_lock
}

type GlobalManager struct {
	TrainerRecLocks map[uint16]*RecordLock
	MapLock sync.Mutex
	GlobalLock *sync.RWMutex
    NumReading int
	NumWritingOrQueued int //currently writing or queued to write
}

/*
Function Name:  GetRecordLock
Description:    method of GlobalManager
				retrieves RecordLock for given trainer id, creates and
                inserts new RecordLock into the manager map if not present
Parameters:     id: trainer record id
Return Value:   referenced RecordLock for that id
Type:           uint16 -> *RecordLock
*/
func (m *GlobalManager) GetRecordLock(id uint16) *RecordLock {
    m.MapLock.Lock()
    rec_lock, ok := m.TrainerRecLocks[id]
    if !ok {
        rec_lock = NewRecordLock()
        m.TrainerRecLocks[id] = rec_lock
    }
    m.MapLock.Unlock()
    return rec_lock
}

/*
Function Name:  RLockRecord
Description:    method of GlobalManager
				acquires reader lock for specified record id, prevents
                global ReadAll from starting while record op begins,
                waits if writers are active or queued
Parameters:     id: trainer record id
Return Value:   n/a
Type:           uint16 -> n/a
*/
func (m *GlobalManager) RLockRecord(id uint16) {
    m.GlobalLock.RLock() //prevent ReadAll from starting while starting record op
    rec_lock := m.GetRecordLock(id)
    rec_lock.Lock.Lock()

    for rec_lock.NumWriting > 0 || rec_lock.WrQueue.Len() > 0 {
        rec_lock.Cond.Wait() //if any writers are active or queued, readers wait
    }
    rec_lock.NumReading++
    rec_lock.Lock.Unlock()
}

/*
Function Name:  RUnlockRecord
Description:    method of GlobalManager
				releases reader lock previously acquired for record id
                awakes waiting writers when last reader exits
Parameters:     id: trainer record id
Return Value:   n/a
Type:           uint16 -> n/a
*/
func (m *GlobalManager) RUnlockRecord(id uint16) {
    rec_lock := m.GetRecordLock(id)
    rec_lock.Lock.Lock()
    if rec_lock.NumReading > 0 {
        rec_lock.NumReading--
    }
    if rec_lock.NumReading == 0 {
        rec_lock.Cond.Broadcast() //awake writers or waiting readers
    }
    rec_lock.Lock.Unlock()
    m.GlobalLock.RUnlock() //release global lock previously taken
}

/*
Function Name:  WLockRecord
Description:    method of GlobalManager
				acquires writer lock for specified record id
                enqueues writer and waits until at head of queue
                prevents writer starvation due to ReadAll (takes exclusive access)
Parameters:     id: trainer record id
Return Value:   n/a
Type:           uint16 -> n/a
*/
func (m *GlobalManager) WLockRecord(id uint16) {
    //block ReadAll from taking exclusive lock while writer progresses
    m.GlobalLock.RLock()
    rec_lock := m.GetRecordLock(id)
    rec_lock.Lock.Lock()
    waiter := rec_lock.WrQueue.PushBack(struct{}{}) //insert blank marker into writer queue

	//conditions for writer to work
	//has to be at head of queue
	//can't have active readers
	//can't have active writer
    for {
        front := rec_lock.WrQueue.Front()
        if front == waiter && rec_lock.NumReading == 0 && rec_lock.NumWriting == 0 {
            break
        }
        rec_lock.Cond.Wait()
    }

    //remove self from queue, mark as writer
    rec_lock.WrQueue.Remove(waiter)
    rec_lock.NumWriting = 1
    rec_lock.Lock.Unlock()
}

/*
Function Name:  WUnlockRecord
Description:    method of GlobalManager
				releases writer lock for specified record id,
                awakes waiting readers or writers
Parameters:     id: trainer record id
Return Value:   n/a
Type:           uint16 -> n/a
*/
func (m *GlobalManager) WUnlockRecord(id uint16) {
    rec_lock := m.GetRecordLock(id)
    rec_lock.Lock.Lock()
    rec_lock.NumWriting = 0

    rec_lock.Cond.Broadcast() //wake next writer or waiting readers
    rec_lock.Lock.Unlock() //release writer lock
    m.GlobalLock.RUnlock() //release global rec_lock taken in WLockRecord
}


/*
Function Name:  LockReadAll
Description:    method of GlobalManager
				blocks until no record ops are active, prevents new
                record ops from starting while caller holds lock
				exclusive global lock for reading all trainer records
Parameters:     n/a
Return Value:   n/a
Type:           n/a -> n/a
*/
func (gm *GlobalManager) LockReadAll() {
    gm.GlobalLock.Lock()
}

/*
Function Name:  UnlockReadAll
Description:    method of GlobalManager
				releases exclusive global lock
Parameters:     n/a
Return Value:   n/a
Type:           n/a -> n/a
*/
func (gm *GlobalManager) UnlockReadAll() {
    gm.GlobalLock.Unlock()
}

/*
Function Name:  NewGlobalManager
Description:    allocates and initialize a GlobalManager containing
                per-record lock map and global read/write mutex,
				used to coordinate ReadAll vs other record ops
Parameters:     N/A
Return Value:   newly allocated and initialized GlobalManager
Type:           n/a -> *GlobalManager
*/
func NewGlobalManager() *GlobalManager {
	m := &GlobalManager{
		TrainerRecLocks: make(map[uint16]*RecordLock),
		GlobalLock: new(sync.RWMutex),
	}
	return m
}

//regexp for client requests
var (
	ReqGetPokeID     = regexp.MustCompile(`^REQ_POKE_ID ([1-9][0-9]*)$`)
	ReqGetTrainerID  = regexp.MustCompile(`^REQ_TRAINER_ID ([1-9][0-9]*)$`)
	ReqGetTrainerAll = regexp.MustCompile(`^REQ_TRAINER_ALL$`)
	//regexp individually captures pokemon ids, if less than 6 then next capture is ""
	ReqPostTrainer = regexp.MustCompile(`^POST_TRAINER (\S+)(?: (\d+))?(?: (\d+))?(?: (\d+))?(?: (\d+))?(?: (\d+))?(?: (\d+))?$`)
	ReqPutTrainer  = regexp.MustCompile(`^PUT_TRAINER (\d+)(?: (\d+))?(?: (\d+))?(?: (\d+))?(?: (\d+))?(?: (\d+))?(?: (\d+))?$`)
	ReqDelTrainer  = regexp.MustCompile(`^DEL_TRAINER (\d+)$`)
	ReqGetLogN     = regexp.MustCompile(`^REQ_LOG_FILE (\d+)$`)
)

type PokeRec struct {
	ID    uint16   //721
	Name  [12]byte //Fletchinder\0
	Type1 [9]byte  //Electric\0
	Type2 [9]byte  //Fighting\0 NULLABLE
	//total is sum of all stats
	HP          uint8    //255 part of total
	Attack      uint8    //165 part of total
	Defense     uint8    //230 part of total
	SpAtk       uint8    //154 part of total
	SpDef       uint8    //230 part of total
	Speed       uint8    //160 part of total
	Generation  uint8    //6
	IsLegendary uint8    //0 false, 1 true
	Color       [7]byte  //Yellow\0
	HasGender   uint8    //0 false, 1 true
	PrMale      uint8    //0-8, must divide by 8
	EggGroup1   [13]byte //Undiscovered\0
	EggGroup2   [11]byte //Human-Like\0 NULLABLE
	HasMegaEvo  uint8    //0 false, 1 true
	HeightM     uint16   //1450 highest, must divide by 100
	WeightKg    uint16   //9500 highest, must divide by 10
	CatchRate   uint8    //255
	BodyStyle   [17]byte //bipedal_tailless\0
}

/*
Function Name:  Print()
Description:    method of PokeRec
Parameters:     N/A
Return Value:   just prints the pokemon record in a nice format
Type:           n/a -> n/a
*/
func (rec PokeRec) Print() {
	fmt.Println("ID:", rec.ID)
	poke_name := fmt.Sprintf("%s", rec.Name)
	fmt.Println(" | Name:", poke_name)
	poke_type1 := fmt.Sprintf("%s", rec.Type1)
	fmt.Println(" | Type 1:", poke_type1)
	if rec.Type2[0] != 0 {
		poke_type2 := fmt.Sprintf("%s", rec.Type2)
		fmt.Println(" | Type 2:", poke_type2)
	} else {
		fmt.Println(" | Type 2: N/A")
	}
	fmt.Printf(" | Total: %d\n", rec.HP+rec.Attack+rec.Defense+rec.SpAtk+rec.SpDef+rec.Speed)
	fmt.Printf(" | -  HP: %d", rec.HP)
	fmt.Printf(" | Attack: %d", rec.Attack)
	fmt.Printf(" | Defense: %d\n", rec.Defense)
	fmt.Printf(" | -  Special attack: %d", rec.SpAtk)
	fmt.Printf(" | Special defense: %d\n", rec.SpDef)
	fmt.Printf(" | -  Speed: %d\n", rec.Speed)
	fmt.Printf(" | Generation: %d\n", rec.Generation)
	if rec.IsLegendary != 0 {
		fmt.Printf(" | Is legendary?: True\n")
	} else {
		fmt.Printf(" | Is legendary?: False\n")
	}
	fmt.Printf(" | Color: %s\n", rec.Color)
	if rec.HasGender != 0 {
		fmt.Printf(" | Has gender?: True\n")
	} else {
		fmt.Printf(" | Has gender?: False\n")
	}
	if rec.HasGender != 0 {
		prob := float32(rec.PrMale) / 8.0
		prob_male := fmt.Sprintf("%.3f", prob)
		fmt.Printf(" | Prob. being male: %s\n", prob_male)
	} else {
		fmt.Printf(" | Prob. being male: N/A\n")
	}
	fmt.Printf(" | Egg Group 1: %s", rec.EggGroup1)
	if rec.EggGroup2[0] != 0 {
		fmt.Printf(" | Egg Group 2: %s\n", rec.EggGroup2)
	} else {
		fmt.Printf(" | Egg Group 2: N/A\n")
	}
	if rec.HasMegaEvo != 0 {
		fmt.Printf(" | Has mega evolution?: True\n")
	} else {
		fmt.Printf(" | Has mega evolution?: False\n")
	}
	fmt.Printf(" | Height (m): %.2f", float32(rec.HeightM)/100.0)
	fmt.Printf(" | Weight (kg): %.1f\n", float32(rec.WeightKg)/10.0)
	fmt.Printf(" | Catch rate: %d\n", rec.CatchRate)
	fmt.Printf(" | Body style: %s\n\n", rec.BodyStyle)
}

type PokeDisplay struct {
	ID   uint16
	Name [12]byte
}

type TrainerRec struct {
	ID    uint16
	Name  [16]byte
	Poke1 PokeDisplay
	Poke2 PokeDisplay
	Poke3 PokeDisplay
	Poke4 PokeDisplay
	Poke5 PokeDisplay
	Poke6 PokeDisplay
}

/*
Function Name:  Print()
Description:    method of TrainerRec
Parameters:     N/A
Return Value:   just prints the trainer record in a nice format
Type:           n/a -> n/a
*/
func (rec TrainerRec) Print() {
	pokemon := []PokeDisplay{
		rec.Poke1,
		rec.Poke2,
		rec.Poke3,
		rec.Poke4,
		rec.Poke5,
		rec.Poke6,
	}

	trainer_name := fmt.Sprintf("%s", rec.Name)
	fmt.Println("ID:", rec.ID)
	fmt.Println(" | Name:", trainer_name)
	fmt.Println(" | Pokemon IDs:")
	for _, poke := range pokemon {
		if poke.ID == 0 {
			break
		}
		fmt.Printf("   | %d (%s)\n", poke.ID, poke.Name)
	}
	fmt.Println()
}

/*
Function Name:  GetPokemon
Description:    seeks in pokemon file for pokemon record by id
Parameters:		poke_file: the pokemon binary data file
				id: the record id to search for
Return Value:   the entire pokemon record if found and error (if any)
Type:           *os.File, uint16 -> PokeRec, error
*/
func GetPokemon(poke_file *os.File, id uint16) (PokeRec, error) {
	var poke PokeRec
	offset := int64(id-1) * int64(unsafe.Sizeof(poke))
	if _, err := poke_file.Seek(offset, 0); err != nil {
		return PokeRec{}, err
	}

	if err := binary.Read(poke_file, binary.LittleEndian, &poke); err != nil {
		return PokeRec{}, err
	} //assumed binary files written on acad

	return poke, nil
}

/*
Function Name:  GetPokeName
Description:	seeks in pokemon file for pokemon name by ID
				used in PostTrainer and PutTrainer
Parameters:		poke_file: the pokemon binary data file
				id: the record ID to search for
Return Value:   the name (bytes) of the pokemon if found and error (if any)
Type:           *os.File, uint16 -> [12]byte, error
*/
func GetPokeName(poke_file *os.File, id uint16) ([12]byte, error) {
	var poke_name [12]byte
	offset := int64(id-1)*int64(unsafe.Sizeof(PokeRec{})) + 2
	if _, err := poke_file.Seek(offset, 0); err != nil {
		return poke_name, err
	}

	if err := binary.Read(poke_file, binary.LittleEndian, &poke_name); err != nil {
		return poke_name, err
	} //assumed binary files written on acad

	return poke_name, nil
}

/*
Function Name:  GetTrainer
Description:    seeks in trainer file for trainer record by ID
Parameters:		trainer_file: the trainer binary data file
				id: the record ID to search for
Return Value:   the entire trainer record if found and error (if any)
Type:           *os.File, uint16 -> TrainerRec, error
*/
func GetTrainer(trainer_file *os.File, id uint16) (TrainerRec, error) {
	var trainer TrainerRec
	offset := int64(id-1) * int64(unsafe.Sizeof(trainer))
	if _, err := trainer_file.Seek(offset, 0); err != nil {
		return TrainerRec{}, err
	}

	if err := binary.Read(trainer_file, binary.LittleEndian, &trainer); err != nil {
		return TrainerRec{}, err
	}
	if trainer.ID == 0 {
		return TrainerRec{}, fmt.Errorf("trainer ID not found")
	}

	return trainer, nil
}

/*
Function Name:  PostTrainer
Description:    creates a new record and appends to end of trainer file
Parameters:		trainer_file: the trainer binary data file
				poke_file: the pokemon binary data file
				name: name of the trainer (15 chars or less)
				pokemon: list of assigned pokemon IDs
Return Value:   the new trainer's id if all pokemon were found and record successfully allocated and error (if any)
Type:           *os.File, *os.File, string, []uint16 -> uint16, error
*/
func PostTrainer(trainer_file *os.File, poke_file *os.File, name string, pokemon []uint16) (uint16, error) {
	var trainer TrainerRec
	trainer_size := int64(unsafe.Sizeof(trainer))
	info, err := trainer_file.Stat()
	if err != nil {
		return 0, err
	}

	file_size := info.Size()
	if file_size%trainer_size != 0 { //gofmt pushes these together?
		return 0, fmt.Errorf("file size is not a multiple of record size")
	}

	next := uint64(file_size/trainer_size) + 1
	if next > 0xFFFF { //max
		return 0, fmt.Errorf("next ID out of range")
	}
	trainer.ID = uint16(next)
	copy(trainer.Name[:], name)

	poke_slots := []*PokeDisplay{
		&trainer.Poke1,
		&trainer.Poke2,
		&trainer.Poke3,
		&trainer.Poke4,
		&trainer.Poke5,
		&trainer.Poke6,
	}

	for idx := 0; idx < len(pokemon) && idx < len(poke_slots); idx++ {
		var display PokeDisplay
		name, err := GetPokeName(poke_file, pokemon[idx])
		if err != nil {
			return 0, fmt.Errorf("pokemon ID not found")
		}
		display.ID = pokemon[idx]
		display.Name = name
		*poke_slots[idx] = display
	} //if there aren't 6, the remaining ids are 0 by default

	if _, err := trainer_file.Seek(0, unix.SEEK_END); err != nil {
		return 0, err
	}
	if err := binary.Write(trainer_file, binary.LittleEndian, &trainer); err != nil {
		return 0, err
	}

	return trainer.ID, trainer_file.Sync()
}

/*
Function Name:  PutTrainer
Description:    seeks for trainer record by ID and modifies pokemon assignment if found
Parameters:		trainer_file: the trainer binary data file
				poke_file: the pokemon binary data file
				id: the record ID to search for
				pokemon: list of new pokemon IDs to assign
Return Value:   nil if trainer was found, pokemon were found, and modification was successful or error
Type:           *os.File, *os.File, uint16, []uint16 -> error
*/
func PutTrainer(trainer_file *os.File, poke_file *os.File, id uint16, pokemon []uint16) error {
	old_data, err := GetTrainer(trainer_file, id)
	if err != nil {
		return fmt.Errorf("trainer ID not found")
	}

	var trainer TrainerRec
	trainer.ID = old_data.ID
	trainer.Name = old_data.Name

	trainer_size := int64(unsafe.Sizeof(trainer))
	info, err := trainer_file.Stat()
	if err != nil {
		return err
	}

	file_size := info.Size()
	if file_size%trainer_size != 0 {
		return fmt.Errorf("file size is not a multiple of record size")
	}

	poke_slots := []*PokeDisplay{
		&trainer.Poke1,
		&trainer.Poke2,
		&trainer.Poke3,
		&trainer.Poke4,
		&trainer.Poke5,
		&trainer.Poke6,
	}

	for idx := range poke_slots {
		if idx < len(pokemon) {
			name, err := GetPokeName(poke_file, pokemon[idx])
			if err != nil {
				return fmt.Errorf("pokemon ID not found")
			}
			poke_name := name
			*poke_slots[idx] = PokeDisplay{ID: pokemon[idx], Name: poke_name}
		} else {
			*poke_slots[idx] = PokeDisplay{}
		}
	}

	offset := int64(id-1) * trainer_size
	if _, err := trainer_file.Seek(offset, 0); err != nil {
		return err
	}

	if err := binary.Write(trainer_file, binary.LittleEndian, &trainer); err != nil {
		return err
	}

	return trainer_file.Sync()
}

/*
Function Name:  DeleteTrainer
Description:    Logically deletes record (zeroed out)
Parameters:		trainer_file: the trainer binary data file
				id: the record ID to search for
Return Value:   nil if trainer found and no other file errors or error
Type:           *os.File, uint16 -> error
*/
func DeleteTrainer(trainer_file *os.File, id uint16) error {
	if _, err := GetTrainer(trainer_file, id); err != nil {
		return err
	}

	var blank TrainerRec
	trainer_size := int64(unsafe.Sizeof(blank))
	info, err := trainer_file.Stat()
	if err != nil {
		return err
	}

	file_size := info.Size()
	if file_size%trainer_size != 0 {
		return fmt.Errorf("file size is not a multiple of record size")
	}

	offset := int64(id-1) * trainer_size
	if _, err := trainer_file.Seek(offset, 0); err != nil {
		return err
	}
	if err := binary.Write(trainer_file, binary.LittleEndian, &blank); err != nil {
		return err
	}

	return nil
}

/*
Function Name:  LogReadN
Description:    reads the last n lines from the log file,
				if file has fewer than n lines, return whole file
Parameters:     log_file: log file to read from
                n: number of lines to return
Return Value:   single newline-terminated string of all requested logs and error (if any)
Type:           *os.File, int -> string, error
*/
func LogReadN(log_file *os.File, n int) (string, error) {
	info, err := log_file.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() == 0 {
		return "Log file empty.", nil
	}

	if _, err := log_file.Seek(0, 0); err != nil {
		return "", err
	}
	data, err := io.ReadAll(log_file)
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(bytes.TrimSuffix(data, []byte{'\n'})), "\n")
	start_idx := 0
	if len(lines) > n {
		start_idx = len(lines) - n
	}
	return strings.Join(lines[start_idx:], "\n") + "\n", nil
}

/*
Function Name:  ReallyWrite
Description:    guarantees that entire message is written to file stream
Parameters:		fp: the file stream (abstract of file descriptor)
				msg: the message to send over stream
Return Value:   nil if all bytes were successfully written or error
Type:           *os.File, string -> error
*/
func ReallyWrite(fp *os.File, msg string) error {
	data := []byte(msg)
	len_buf := make([]byte, 4)
	binary.BigEndian.PutUint32(len_buf, uint32(len(data))) //network byte order

	packet := append(len_buf, data...)
	total := 0
	for total < len(packet) {
		bytes_written, err := fp.Write(packet[total:])
		if err != nil {
			return err
		}
		total += bytes_written
	}
	return nil
}

/*
Function Name:  ReallyRead
Description:    guarantees that entire message is read from file stream
Parameters:     fp: the file stream (abstract of file descriptor)
Return Value:   the message read from file stream or error
Type:           *os.File -> string, error
*/
func ReallyRead(fp *os.File) (string, error) {
	len_buf := make([]byte, 4)
	total := 0
	for total < 4 { //lenth up to 4 bytes
		bytes_read, err := fp.Read(len_buf[total:])
		if err != nil {
			return "", err
		}
		total += bytes_read
	}

	length := binary.BigEndian.Uint32(len_buf)
	msg := make([]byte, length)
	total = 0
	for total < int(length) {
		bytes_read, err := fp.Read(msg[total:])
		if err != nil {
			return "", err
		}
		total += bytes_read
	}
	return string(msg), nil
}
