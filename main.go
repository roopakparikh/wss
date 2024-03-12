/*
*
* This work is based on
* http://www.brendangregg.com/wss.pl
* Re-written in golang for better integration with rest of Platform9 stack
* Requirements: Linux 4.3+
* USAGE: wss PID duration

  - COLUMNS:
  - - Est(s):  Estimated WSS measurement duration: this accounts for delays
  - with setting and reading pagemap data, which inflates the
  - intended sleep duration.
  - - Ref(MB): Referenced (Mbytes) during the specified duration.
  - This is the working set size metric.
    *
  - WARNING: This tool sets and reads system and process page flags, which can
  - take over one second of CPU time, during which application may experience
  - slightly higher latency (eg, 5%). Consider these overheads. Also, this is
  - activating some new kernel code added in Linux 4.3 that you may have never
  - executed before. As is the case for any such code, there is the risk of
  - undiscovered kernel panics (I have no specific reason to worry, just being
  - paranoid). Test in a lab environment for your kernel versions, and consider
  - this experimental: use at your own risk.
    *
  - Copyright 2018 Netflix, Inc.
  - Licensed under the Apache License, Version 2.0 (the "License")
    *
  - 13-Jan-2018	Brendan Gregg	Created this.
  - 10-Mar-2024  Platform9 Systems Inc created a golang version of the same
*/
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
	"unsafe"
)

// see Documentation/vm/pagemap.txt:
// also https://fivelinesofcode.blogspot.com/2014/03/how-to-translate-virtual-to-physical.html
// and also https://www.kernel.org/doc/Documentation/vm/idle_page_tracking.txt

const (
	NUM_BYTE_64        uint64 = 8
	PFN_MASK                  = uint64(1)<<55 - 1
	PATHSIZE                  = 128
	LINESIZE                  = 256
	PAGEMAP_CHUNK_SIZE        = 8
	IDLEMAP_CHUNK_SIZE        = 8
	IDLEMAP_BUF_SIZE          = 4096

	// big enough to span 740 GB (TODO check if this is enough)
	MAX_IDLEMAP_SIZE = 20 * 1024 * 1024

	// Following two constants should come from some linux headers, but hardcoded there
	// from mm/page_idle.c
	BITMAP_CHUNK_SIZE = 8
	PAGE_OFFSET       = 0xffff880000000000
)

// globals
var (
	g_debug       = 0 // 1 == some, 2==verbose
	g_activepages = 0
	g_walkedpages = 0
	g_idlepath    = "/sys/kernel/mm/page_idle/bitmap"
	g_idlebuf     = make([]uint64, MAX_IDLEMAP_SIZE)
	g_idlebufsize uint64
)

/*
 * This code must operate on bits in the pageidle bitmap and process pagemap.
 * Doing this one by one via syscall read/write on a large process can take too
 * long, eg, 7 minutes for a 130 Gbyte process. Instead, I copy (snapshot) the
 * idle bitmap and pagemap into our memory with the fewest syscalls allowed,
 * and then process them with load/stores. Much faster, at the cost of some memory.
 */
func mapidle(pid int, mapstart, mapend uint64) error {

	var offset, pfn, idlemapp, idlebits, i uint64

	pagesize := os.Getpagesize()

	pagebufsize := (PAGEMAP_CHUNK_SIZE * (mapend - mapstart)) / uint64(pagesize)

	// create a pagebuf which is equivalent to pagebufsize in bytes
	pagebuf := make([]uint64, pagebufsize)

	// open pagemap for virtual to PFN translation
	pagepath := fmt.Sprintf("/proc/%d/pagemap", pid)

	pagefd, err := os.Open(pagepath)
	if err != nil {
		return fmt.Errorf("Can't read pagemap file %s", err)
	}

	defer pagefd.Close()
	// cache pagemap to get PFN, then operate on PFN from idlemap
	offset = PAGEMAP_CHUNK_SIZE * mapstart / uint64(pagesize)

	if _, err := pagefd.Seek(int64(offset), 0); err != nil {
		return fmt.Errorf("Can't seek pagemap file %s", err)
	}

	// optimized: read this in one syscall, but do we need to read the file again and gain till the
	// length == the bytes read ?
	read, err := pagefd.Read((*(*[]byte)(unsafe.Pointer(&pagebuf)))[:])
	if err != nil {
		return fmt.Errorf("Read page map failed %s", err)
	}
	if read <= 0 {
		return fmt.Errorf("Read page map failed only read %d", read)
	}

	// reading
	// 1 unint64 is 8 bytes
	for i = 0; i < pagebufsize/8; i++ {

		// convert virtual address p to physical PFN
		//pfn = binary.LittleEndian.Uint64(pagebuf[i]) & PFN_MASK
		pfn = pagebuf[i] & PFN_MASK
		if pfn == 0 {
			continue
		}
		// read idle bit
		idlemapp = (pfn / 64) * BITMAP_CHUNK_SIZE
		if ((idlemapp) > g_idlebufsize) || ((idlemapp) > uint64(len(g_idlebuf))) {
			return fmt.Errorf("ERROR: bad PFN read from page map. read %d and buf size  %d, buf len %d", idlemapp, g_idlebufsize, len(g_idlebuf))
		}

		if g_debug != 0 {
			fmt.Printf("Mapping idle page idlebuf %d idlemapp start %d idlemapp end %d \n", g_idlebufsize, idlemapp, idlemapp+NUM_BYTE_64)
		}

		//idlebits = binary.LittleEndian.Uint64(g_idlebuf[idlemapp : idlemapp+NUM_BYTE_64])
		idlebits = g_idlebuf[idlemapp]
		if g_debug > 1 {
			fmt.Printf("R: p %llx pfn %llx idlebits %llx\n", pagebuf[i], pfn, idlebits)
		}
		if idlebits&(1<<(pfn%64)) == 0 {
			g_activepages++
		}
		g_walkedpages++
	}
	return nil
}

func walkmaps(pid int) error {

	// read virtual mappings
	mapsfile, err := os.OpenFile(fmt.Sprintf("/proc/%d/maps", pid), os.O_RDONLY, 0644)
	if err != nil {
		return fmt.Errorf("Can't read maps file %s", err)
	}
	linescanner := bufio.NewScanner(mapsfile)

	defer mapsfile.Close()

	for linescanner.Scan() {
		var mapstart, mapend uint64
		line := linescanner.Text()
		_, err := fmt.Sscanf(line, "%x-%x", &mapstart, &mapend)
		if err != nil {
			return fmt.Errorf("Error parsing line %s, err %s", line, err)
		}
		if g_debug != 0 {
			fmt.Printf("MAP %x-%x\n", mapstart, mapend)
		}
		if mapstart > PAGE_OFFSET {
			continue // page idle tracking is user mem only
		}
		err = mapidle(pid, mapstart, mapend)
		if err != nil {
			return fmt.Errorf("Error setting map %x-%x. Exiting. \n%s\n", mapstart, mapend, err)
		}
	}
	if err := linescanner.Err(); err != nil {
		return fmt.Errorf("Error reading standard input: %s", err)
	}

	return nil
}

func setidlemap() error {

	idlefd, err := os.OpenFile(g_idlepath, os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("Can't write idlemap file %s", err)
	}
	defer idlefd.Close()

	buf := make([]byte, IDLEMAP_BUF_SIZE)
	for i := 0; i < IDLEMAP_BUF_SIZE; i++ {
		buf[i] = 0xff
	}
	// set entire idlemap flags
	for {
		_, err := idlefd.Write(buf)
		if err != nil {
			break
		}
	}
	return nil
}


func loadidlemap() error {
	idlefd, err := os.OpenFile(g_idlepath, os.O_RDONLY, 0644)
	if err != nil {
		return fmt.Errorf("Can't read idlemap file %s", err)
	}
	defer idlefd.Close()
	count := 0
	for {
		n, err := idlefd.Read((*(*[]byte)(unsafe.Pointer(&g_idlebuf)))[:])
		if err != nil {
			if err != io.EOF {
				return fmt.Errorf("Error reading file %s", err)
			}
			break
		}
		count = count + n
		g_idlebufsize += uint64(n)
	}
	if g_debug != 0 {
		fmt.Printf("Size of the buffer %d, idlebufsize%d \n", len(g_idlebuf), g_idlebufsize)
	}
	return nil
}

func main() {
	pid, _ := strconv.Atoi(os.Args[1])
	duration, _ := strconv.ParseFloat(os.Args[2], 64)
	var ts1, ts2, ts3, ts4 time.Time
	var set_us, read_us, dur_us, slp_us, est_us int64
	// options
	if len(os.Args) < 3 {
		fmt.Println("USAGE: wss PID duration(s)")
		os.Exit(0)
	}
	if duration < 0.01 {
		fmt.Println("Interval too short. Exiting.")
		os.Exit(1)
	}
	fmt.Printf("Watching PID %d page references during %.2f seconds...\n", pid, duration)
	// set idle flags
	ts1 = time.Now()
	err := setidlemap()
	if err != nil {
		fmt.Printf("Error setting idle map  %s", err)
		return
	}
	// sleep
	ts2 = time.Now()
	time.Sleep(time.Duration(duration * float64(time.Second)))
	ts3 = time.Now()
	// read idle flags
	err = loadidlemap()
	if err != nil {
		fmt.Printf("Error loading idle map  %s", err)
		return
	}
	err = walkmaps(pid)
	if err != nil {
		fmt.Printf("Error walking map  %s", err)
		return
	}
	ts4 = time.Now()
	// calculate times
	set_us = int64(ts2.Sub(ts1).Seconds() * 1000000)
	slp_us = int64(ts3.Sub(ts2).Seconds() * 1000000)
	read_us = int64(ts4.Sub(ts3).Seconds() * 1000000)
	dur_us = int64(ts4.Sub(ts1).Seconds() * 1000000)
	est_us = dur_us - (set_us / 2) - (read_us / 2)
	if g_debug != 0 {
		fmt.Printf("set time  : %.3f s\n", float64(set_us)/1000000)
		fmt.Printf("sleep time: %.3f s\n", float64(slp_us)/1000000)
		fmt.Printf("read time : %.3f s\n", float64(read_us)/1000000)
		fmt.Printf("dur time  : %.3f s\n", float64(dur_us)/1000000)
		// assume getpagesize() sized pages:
		fmt.Printf("referenced: %d pages, %d Kbytes\n", g_activepages, g_activepages*os.Getpagesize())
		fmt.Printf("walked    : %d pages, %d Kbytes\n", g_walkedpages, g_walkedpages*os.Getpagesize())
	}
	// assume getpagesize() sized pages:
	mbytes := float64(g_activepages*os.Getpagesize()) / (1024 * 1024)
	fmt.Printf("%-7s %10s\n", "Est(s)", "Ref(MB)")
	fmt.Printf("%-7.3f %10.2f", float64(est_us)/1000000, mbytes)
	os.Exit(0)
}
