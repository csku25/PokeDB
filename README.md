# PokeDB
- Asynchronous Pokemon Database Server and Client
- All written in Go
- Database isn't actually valid, just CSV I converted to (a quite large) binary
## Background
This was a project I accumulated and submitted as my final for a
grad level course. Please, seriously, give the most raw criticism.

## Docs
- Doc comments are provided in the code
- The client commands are displayed by typing 'help'

### Delete Request
Upon a client requesting to delete a trainer, I chose to follow recommended procedure.
Database servers logically delete records first, then a background process cleans up
and physically removes record from disk. In lieu of a background process to physically
delete records, the zeroed out record remains in the trainer binary data file and
all library functions related to trainer record data were updated to handle this.
Though GetTrainer receives EOF when reading, the function returns a different error
to indicate that the record being searched for is not in the trainer file because it
knows there are more records that come after it. (based off of file size)

### Mutual Exlusion Design
My implementation uses a per-record lock manager with RecordLock structs
containing mutexes, condition variables, and writer queues to enable concurrent
trainer record access. Multiple readers can access records simultaneously while
writers have exclusive access, and the writer queue prevents starvation through
FIFO ordering. The global RWMutex coordinates ReadAll operations with single record
operations to ensure there will be consistency between all file access. I strived to
prevent writer starvation with a strong writer's preference and I also made sure 
deadlocks were not possible through an explicit locking order. I recognize however that
this complex locking design increases the risk where ReadAll can block all record
operations and can create a severe bottleneck. Ideally there would be an automatic
cleanup for the locking methods. Condition variable signaling issues could also arise
and cause unnecessary waiting. Overall my design sacrifices some simplicity for
concurrency which also makes it prone to subtle bugs. In testing with a few clients,
I never saw any issues with simultaneously creating or modifying different trainer ids.

### Logging
The logging system uses a single mutex to protect a log file that outputs to both stdout
and the logging text file via Go's MultiWriter, providing atomic log operations and
dual output with very little overhead. I chose to only log the client connections,
the requests made, as well as when the client disconnects or when server shuts down.
All errors are printed to stdout for the user who has control of the server.
LogReadN retrieves the last N entries for client queries under mutex protection.
I also realize that there is the issue of the log file just appending to the end, in
which LogReadN inefficiently reads the entire file in O(file size) time. I noticed
while testing out the mutual exclusivity that the logs get very big very quick. I also
want to state that the client prints the log to stdout upon request instead of writing
to its own log file copy.
