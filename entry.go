package logrus

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"tlog.app/go/loc"
)

var (

	// Positions in the call stack when tracing to report the calling method
	minimumCallerDepth int

	// Used for caller information initialisation
	callerInitOnce sync.Once
)

const (
	logrusPackage          = "github.com/sirupsen/logrus/"
	maximumCallerDepth int = 4
	knownLogrusFrames  int = 2
)

func init() {
	// start at the bottom of the stack before the package-name cache is primed
	minimumCallerDepth = 1
}

// Defines the key when adding errors using WithError.
var ErrorKey = "error"

// An entry is the final or intermediate Logrus logging entry. It contains all
// the fields passed with WithField{,s}. It's finally logged when Trace, Debug,
// Info, Warn, Error, Fatal or Panic is called on it. These objects can be
// reused and passed around as much as you wish to avoid field duplication.
type Entry struct {
	Logger *Logger

	// Contains all the fields set by the user.
	Data FieldsSlice
}

type FieldsSlice []Field

func (s FieldsSlice) ToFields() Fields {
	fields := make(Fields, len(s))
	for _, f := range s {
		fields[f.Key] = f.Value
	}
	return fields
}

func (s FieldsSlice) Get(key string) any {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i].Key == key {
			return s[i].Value
		}
	}
	return nil
}

type Field struct {
	Key   string
	Value any
}

func NewEntry(logger *Logger) *Entry {
	entry := &Entry{
		Logger: logger,
	}
	return entry
}

func (entry *Entry) Dup() *Entry {
	newEntry := *entry
	entry.Data = make([]Field, len(entry.Data))
	copy(entry.Data, entry.Data)
	return &newEntry
}

func (entry *Entry) dupWithRoom(n int) *Entry {
	newEntry := *entry
	newEntry.Data = make([]Field, len(entry.Data), len(entry.Data)+n)
	copy(newEntry.Data, entry.Data)
	return &newEntry
}

// Returns the bytes representation of this entry from the formatter.
func (entry *Entry) Bytes() ([]byte, error) {
	return entry.Logger.Formatter.Format(entry)
}

// Returns the string representation from the reader and ultimately the
// formatter.
func (entry *Entry) String() (string, error) {
	serialized, err := entry.Bytes()
	if err != nil {
		return "", err
	}
	str := string(serialized)
	return str, nil
}

// Add an error as single field (using the key defined in ErrorKey) to the Entry.
func (entry *Entry) WithError(err error) *Entry {
	return entry.WithField(ErrorKey, err)
}

// Add a context to the Entry.
func (entry *Entry) WithContext(ctx context.Context) *Entry {
	newEntry := entry.Dup()
	//newEntry.Context = ctx
	return newEntry
}

// Add a single field to the Entry.
func (entry *Entry) WithField(key string, value interface{}) *Entry {
	newEntry := entry.dupWithRoom(1)
	newEntry.Data = append(newEntry.Data, Field{Key: key, Value: value})
	return newEntry
}

// Add a map of fields to the Entry.
func (entry *Entry) WithFields(fields Fields) *Entry {
	newEntry := entry.dupWithRoom(len(fields))
	newEntry.addFields(fields)
	return newEntry
}

func (entry *Entry) addFields(fields Fields) {
	for k, v := range fields {
		entry.Data = append(entry.Data, Field{Key: k, Value: v})
	}
}

// Overrides the time of the Entry.
func (entry *Entry) WithTime(t time.Time) *Entry {
	return entry
}

// getPackageName reduces a fully qualified function name to the package name
// There really ought to be to be a better way...
func getPackageName(f string) string {
	for {
		lastPeriod := strings.LastIndex(f, ".")
		lastSlash := strings.LastIndex(f, "/")
		if lastPeriod > lastSlash {
			f = f[:lastPeriod]
		} else {
			break
		}
	}

	return f
}

// getCaller retrieves the name of the first non-logrus calling function
func getCaller() (string, int) {
	var pcsbuf [maximumCallerDepth]loc.PC
	pcs := loc.CallersFill(knownLogrusFrames, pcsbuf[:])
	for _, pc := range pcs {
		_, file, line := pc.NameFileLine()
		//fmt.Printf("file: %s, line: %d\n", file, line)
		if strings.HasPrefix(file, logrusPackage) {
			continue
		}
		return path.Base(file), line
	}

	// if we got here, we failed to find the caller's context
	return "", 0
}

func (entry *Entry) HasCaller() (has bool) {
	return false
}

func (entry *Entry) log(level Level, msg string, fileName string, lineNo int) {
	const desiredTimeFormat = "2006-01-02 15:04:05.000"
	const desiredTimeLen = len(desiredTimeFormat)

	b := bufferPool.Get()
	b.Grow(desiredTimeLen + 32 + len(fileName) + len(msg) + len(entry.Data)*32)
	{
		buf := b.AvailableBuffer()
		// Want "2006-01-02 15:04:05.000" but the formatter has an optimised
		// impl of RFC3339Nano, which we can easily tweak into our format.
		const tPos = len("2006-01-02T") - 1
		buf = time.Now().AppendFormat(buf, time.RFC3339Nano)[:desiredTimeLen]
		buf[tPos] = ' '
		_, _ = b.Write(buf)
	}
	b.WriteString(levelStrings[level])
	//if f.Component != "" {
	//	b.WriteString(f.Component)
	//	b.WriteByte('/')
	//}
	b.WriteString(fileName)
	b.WriteByte(' ')
	{
		buf := b.AvailableBuffer()
		strconv.AppendInt(buf, int64(lineNo), 10)
		_, _ = b.Write(buf)
	}
	b.WriteString(": ")
	b.WriteString(msg)
	appendKVsAndNewLine(b, entry.Data)

	entry.Logger.mu.Lock()
	defer entry.Logger.mu.Unlock()
	if _, err := entry.Logger.Out.Write(b.Bytes()); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Failed to write to log, %v\n", err)
	}
	bufferPool.Put(b)

	// To avoid Entry#log() returning a value that only would make sense for
	// panic() to use in Entry#Panic(), we avoid the allocation by checking
	// directly here.
	if level <= PanicLevel {
		panic(entry)
	}
}

func (entry *Entry) getBufferPool() (pool BufferPool) {
	if entry.Logger.BufferPool != nil {
		return entry.Logger.BufferPool
	}
	return bufferPool
}

//
//func (entry *Entry) fireHooks() {
//	var tmpHooks LevelHooks
//	entry.Logger.mu.Lock()
//	tmpHooks = make(LevelHooks, len(entry.Logger.Hooks))
//	for k, v := range entry.Logger.Hooks {
//		tmpHooks[k] = v
//	}
//	entry.Logger.mu.Unlock()
//
//	err := tmpHooks.Fire(entry.Level, entry)
//	if err != nil {
//		fmt.Fprintf(os.Stderr, "Failed to fire hook: %v\n", err)
//	}
//}

func (entry *Entry) write() {
	entry.Logger.mu.Lock()
	defer entry.Logger.mu.Unlock()
	serialized, err := entry.Logger.Formatter.Format(entry)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Failed to obtain reader, %v\n", err)
		return
	}
	if _, err := entry.Logger.Out.Write(serialized); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Failed to write to log, %v\n", err)
	}
}

var levelStrings []string

func init() {
	levelStrings = make([]string, len(AllLevels))
	for _, level := range AllLevels {
		levelStrings[level] = fmt.Sprintf(" [%s][%d] ", strings.ToUpper(level.String()), os.Getpid())
	}
}

const FileNameUnknown = "<nil>"

// appendKeysAndNewLine writes the KV pairs attached to the entry to the end of the buffer, then
// finishes it with a newline.
func appendKVsAndNewLine(b *bytes.Buffer, data FieldsSlice) {
	if len(data) == 0 {
		b.WriteByte('\n')
		return
	}

	// Sort the fields by key for consistent output.
	var sortedFields []Field
	if len(data) <= 16 {
		// Avoid allocations for small number of fields.  The fixed-size array
		// gets allocated on the stack, whereas make() does heap allocation
		// if the length isn't known at compile time.
		var dataArr [16]Field
		sortedFields = dataArr[:len(data)]
	} else {
		sortedFields = make([]Field, len(data))
	}
	copy(sortedFields, data)
	slices.SortStableFunc(sortedFields, func(a, b Field) int {
		return strings.Compare(b.Key, a.Key) // reverse order
	})

	lastKey := ""
	for i := len(sortedFields) - 1; i >= 0; i-- {
		key := sortedFields[i].Key
		if key == lastKey || key == "__flush__" {
			// Skip repeat keys.  We iterate in reverse order so these are overwritten values.
			continue
		}
		lastKey = key

		value := sortedFields[i].Value
		b.WriteByte(' ')
		b.WriteString(key)
		b.WriteByte('=')

		switch value := value.(type) {
		case string:
			buf := b.AvailableBuffer()
			buf = strconv.AppendQuote(buf, value)
			b.Write(buf)
		case error:
			buf := b.AvailableBuffer()
			buf = strconv.AppendQuote(buf, value.Error())
			b.Write(buf)
		case fmt.Stringer:
			// Trust the value's String() method.
			buf := b.AvailableBuffer()
			buf = strconv.AppendQuote(buf, value.String())
			b.Write(buf)
		default:
			// No string method, use %#v to get a more thorough dump.
			_, _ = fmt.Fprintf(b, "%#v", value)
		}
	}

	b.WriteByte('\n')
}

// Log will log a message at the level given as parameter.
// Warning: using Log at Panic or Fatal level will not respectively Panic nor Exit.
// For this behaviour Entry.Panic or Entry.Fatal should be used instead.
func (entry *Entry) Log(level Level, args ...interface{}) {
	if entry.Logger.IsLevelEnabled(level) {
		fileName, lineNo := getCaller()
		msg := flattenArgs(args...)
		entry.log(level, msg, fileName, lineNo)
	}
}

func flattenArgs(args ...interface{}) string {
	var msg string
	if len(args) == 1 {
		if s, ok := args[0].(string); ok {
			// Mainline case, one argument that is a string: avoid an alloc.
			msg = s
		} else {
			msg = fmt.Sprint(args[0])
		}
	} else {
		msg = fmt.Sprint(args...)
	}
	return msg
}

func (entry *Entry) Trace(args ...interface{}) {
	entry.Log(TraceLevel, args...)
}

func (entry *Entry) Debug(args ...interface{}) {
	entry.Log(DebugLevel, args...)
}

func (entry *Entry) Print(args ...interface{}) {
	entry.Info(args...)
}

func (entry *Entry) Info(args ...interface{}) {
	if !entry.Logger.IsLevelEnabled(InfoLevel) {
		return
	}

	// Look up the calling function.  Inlined because runtime.Callers() is
	// faster if it doesn't have to walk so far up the stack.
	var fileName string
	var lineNo int
	{
		var n int
		var pc uintptr
		{
			// Use a pool of slices to avoid leaking the slice to the heap.
			// runtime.Callers() is not marked as "noescape".
			pcCache := pcSlicePool[rand.Intn(len(pcSlicePool))]
			pcCache.lock.Lock()
			pcSlice := pcCache.pc
			n = runtime.Callers(2, pcSlice)
			pc = pcSlice[0] // Copy the PC out before we release the lock.
			pcSlice[0] = 0
			pcCache.lock.Unlock()
		}

		if n == 1 {
			// To avoid another allocation, we cache the calculated file and
			// line number in a map.
			cachedCallersMu.RLock()
			info, ok := cachedCallers[pc]
			cachedCallersMu.RUnlock()
			if ok {
				// Fast path: got a hit in the cache.
				fileName, lineNo = info.file, info.line
			} else {
				// Slow path, look up the file/line number.  We reconstruct
				// the pointer slice, just so we can return the above slice
				// to the pool as quickly as possible (and avoid overlapping
				// locks).
				frame, _ := runtime.CallersFrames([]uintptr{pc}).Next()
				fileName = path.Base(frame.File)
				lineNo = frame.Line
				cachedCallersMu.Lock()
				if len(cachedCallers) > 2<<16 {
					// If the cache is getting too big, start dropping entries.
					dropped := 0
					// Go's map iteration is random so deleting the first
					// entries we see should do the trick.
					for k := range cachedCallers {
						delete(cachedCallers, k)
						dropped++
						if dropped > 128 {
							break
						}
					}
				}
				cachedCallers[pc] = callerInfo{fileName, lineNo}
				cachedCallersMu.Unlock()
			}
		}
	}

	entry.log(InfoLevel, flattenArgs(args...), fileName, lineNo)
}

var (
	cachedCallersMu sync.RWMutex
	cachedCallers   = map[uintptr]callerInfo{}
)

type callerInfo struct {
	file string
	line int
}

var pcSlicePool [32]*struct {
	lock sync.Mutex
	pc   []uintptr
}

func init() {
	for i := range pcSlicePool {
		pcSlicePool[i] = &struct {
			lock sync.Mutex
			pc   []uintptr
		}{
			pc: make([]uintptr, 1),
		}
	}
}

func callerFileLine(skip int) (file string, line int) {
	pcCache := pcSlicePool[rand.Intn(len(pcSlicePool))]
	pcCache.lock.Lock()
	defer pcCache.lock.Unlock()

	pc := pcCache.pc
	n := runtime.Callers(skip+2, pc)
	if n < 1 {
		return
	}

	cachedCallersMu.Lock()
	defer cachedCallersMu.Unlock()
	info, ok := cachedCallers[pc[0]]
	if ok {
		return info.file, info.line
	}

	frame, _ := runtime.CallersFrames(pc).Next()
	base := path.Base(frame.File)
	cachedCallers[pc[0]] = callerInfo{base, frame.Line}
	return base, frame.Line
}

func (entry *Entry) Warn(args ...interface{}) {
	entry.Log(WarnLevel, args...)
}

func (entry *Entry) Warning(args ...interface{}) {
	entry.Warn(args...)
}

func (entry *Entry) Error(args ...interface{}) {
	entry.Log(ErrorLevel, args...)
}

func (entry *Entry) Fatal(args ...interface{}) {
	entry.Log(FatalLevel, args...)
	entry.Logger.Exit(1)
}

func (entry *Entry) Panic(args ...interface{}) {
	entry.Log(PanicLevel, args...)
}

// Entry Printf family functions

func (entry *Entry) Logf(level Level, format string, args ...interface{}) {
	if entry.Logger.IsLevelEnabled(level) {
		entry.Log(level, fmt.Sprintf(format, args...))
	}
}

func (entry *Entry) Tracef(format string, args ...interface{}) {
	entry.Logf(TraceLevel, format, args...)
}

func (entry *Entry) Debugf(format string, args ...interface{}) {
	entry.Logf(DebugLevel, format, args...)
}

func (entry *Entry) Infof(format string, args ...interface{}) {
	entry.Logf(InfoLevel, format, args...)
}

func (entry *Entry) Printf(format string, args ...interface{}) {
	entry.Infof(format, args...)
}

func (entry *Entry) Warnf(format string, args ...interface{}) {
	entry.Logf(WarnLevel, format, args...)
}

func (entry *Entry) Warningf(format string, args ...interface{}) {
	entry.Warnf(format, args...)
}

func (entry *Entry) Errorf(format string, args ...interface{}) {
	entry.Logf(ErrorLevel, format, args...)
}

func (entry *Entry) Fatalf(format string, args ...interface{}) {
	entry.Logf(FatalLevel, format, args...)
	entry.Logger.Exit(1)
}

func (entry *Entry) Panicf(format string, args ...interface{}) {
	entry.Logf(PanicLevel, format, args...)
}

// Entry Println family functions

func (entry *Entry) Logln(level Level, args ...interface{}) {
	if entry.Logger.IsLevelEnabled(level) {
		entry.Log(level, entry.sprintlnn(args...))
	}
}

func (entry *Entry) Traceln(args ...interface{}) {
	entry.Logln(TraceLevel, args...)
}

func (entry *Entry) Debugln(args ...interface{}) {
	entry.Logln(DebugLevel, args...)
}

func (entry *Entry) Infoln(args ...interface{}) {
	entry.Logln(InfoLevel, args...)
}

func (entry *Entry) Println(args ...interface{}) {
	entry.Infoln(args...)
}

func (entry *Entry) Warnln(args ...interface{}) {
	entry.Logln(WarnLevel, args...)
}

func (entry *Entry) Warningln(args ...interface{}) {
	entry.Warnln(args...)
}

func (entry *Entry) Errorln(args ...interface{}) {
	entry.Logln(ErrorLevel, args...)
}

func (entry *Entry) Fatalln(args ...interface{}) {
	entry.Logln(FatalLevel, args...)
	entry.Logger.Exit(1)
}

func (entry *Entry) Panicln(args ...interface{}) {
	entry.Logln(PanicLevel, args...)
}

// Sprintlnn => Sprint no newline. This is to get the behavior of how
// fmt.Sprintln where spaces are always added between operands, regardless of
// their type. Instead of vendoring the Sprintln implementation to spare a
// string allocation, we do the simplest thing.
func (entry *Entry) sprintlnn(args ...interface{}) string {
	msg := fmt.Sprintln(args...)
	return msg[:len(msg)-1]
}
