// Copyright (c) 2026 Nawakarit
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License v3.0.
package main

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

// ชื่อ virtual sink ที่ OBS จะ capture เสียงจาก monitor ของมัน
const sinkName = "OBS_Record"

// SinkInput เก็บข้อมูล audio stream หนึ่งตัวจาก `pactl list sink-inputs`
type SinkInput struct {
	ID        int
	ProcessID int
	AppName   string
}

func runCmd(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// sinkExists ตรวจสอบว่า virtual sink มีอยู่แล้วหรือยัง
func sinkExists() bool {
	out, err := runCmd("pactl", "list", "sinks", "short")
	if err != nil {
		return false
	}
	return strings.Contains(out, sinkName)
}

// ensureSink สร้าง null-sink ถ้ายังไม่มี (idempotent)
func ensureSink() error {
	if sinkExists() {
		return nil
	}
	_, err := runCmd("pactl", "load-module", "module-null-sink",
		fmt.Sprintf("sink_name=%s", sinkName),
		fmt.Sprintf("sink_properties=device.description=%s", sinkName))
	return err
}

// รูปแบบบรรทัดของ `wmctrl -lx`:
// <window_id> <desktop> <WM_CLASS> <hostname> <title...>
var wmctrlLineRe = regexp.MustCompile(`^(\S+)\s+(-?\d+)\s+(\S+)\s+(\S+)\s+(.*)$`)

// findPiPWindowPID มองหาหน้าต่าง Picture-in-Picture ของ Firefox
// สังเกตจาก WM_CLASS ที่ขึ้นต้นด้วย "Toolkit" (คงที่ไม่ว่า UI จะภาษาอะไร)
// แล้วคืน PID ของหน้าต่างนั้น
func findPiPWindowPID() (int, bool, error) {
	out, err := runCmd("wmctrl", "-lx")
	if err != nil {
		return 0, false, err
	}
	for _, line := range strings.Split(out, "\n") {
		m := wmctrlLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		windowID, class := m[1], m[3]
		if !strings.Contains(strings.ToLower(class), "toolkit") {
			continue
		}
		pidOut, err := runCmd("xdotool", "getwindowpid", windowID)
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(pidOut))
		if err != nil {
			continue
		}
		return pid, true, nil
	}
	return 0, false, nil
}

var sinkInputHeaderRe = regexp.MustCompile(`(?m)^Sink Input #(\d+)`)
var processIDRe = regexp.MustCompile(`application\.process\.id = "(\d+)"`)
var appNameRe = regexp.MustCompile(`application\.name = "([^"]*)"`)

// listSinkInputs อ่าน audio stream ทั้งหมดที่กำลังเล่นอยู่ตอนนี้
func listSinkInputs() ([]SinkInput, error) {
	out, err := runCmd("pactl", "list", "sink-inputs")
	if err != nil {
		return nil, err
	}
	idxs := sinkInputHeaderRe.FindAllStringSubmatchIndex(out, -1)
	var result []SinkInput
	for i, loc := range idxs {
		start := loc[0]
		end := len(out)
		if i+1 < len(idxs) {
			end = idxs[i+1][0]
		}
		block := out[start:end]

		id, _ := strconv.Atoi(out[loc[2]:loc[3]])
		si := SinkInput{ID: id}
		if pm := processIDRe.FindStringSubmatch(block); pm != nil {
			si.ProcessID, _ = strconv.Atoi(pm[1])
		}
		if am := appNameRe.FindStringSubmatch(block); am != nil {
			si.AppName = am[1]
		}
		result = append(result, si)
	}
	return result, nil
}

// moveSinkInput ย้าย audio stream ไปยัง sink ปลายทาง
func moveSinkInput(id int, sink string) error {
	_, err := runCmd("pactl", "move-sink-input", strconv.Itoa(id), sink)
	return err
}

// ---------- ส่วน GUI ----------

type recorderApp struct {
	win      fyne.Window
	status   *widget.Label
	logBox   *widget.Entry
	startBtn *widget.Button
	stopBtn  *widget.Button

	cancel context.CancelFunc
	mu     sync.Mutex
	routed map[int]bool // sink-input id ที่ถูกย้ายไปแล้ว กันย้ำซ้ำ

	sessionActive bool // true ระหว่างที่หน้าต่าง PiP ยังเปิดอยู่ต่อเนื่อง
	fallbackUsed  bool // กันไม่ให้ fallback สุ่มหยิบซ้ำหลายรอบในเซสชันเดียวกัน
}

// หมายเหตุ: การอัปเดต widget จาก goroutine พื้นหลังแบบตรงๆ ใน Fyne
// เวอร์ชันเก่าทำได้ในทางปฏิบัติ แต่ถ้าใช้ Fyne >= 2.5 แนะนำห่อด้วย fyne.Do(...)
// เพื่อความปลอดภัยด้าน thread เต็มรูปแบบ
func (a *recorderApp) appendLog(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	a.logBox.SetText(a.logBox.Text + msg + "\n")
}

func (a *recorderApp) setStatus(text string) {
	a.status.SetText(text)
}

func (a *recorderApp) start() {
	if err := ensureSink(); err != nil {
		a.appendLog("สร้าง sink ไม่สำเร็จ: %v", err)
		return
	}
	a.appendLog("สร้าง/ยืนยัน sink %s เรียบร้อย", sinkName)
	a.setStatus("สถานะ: กำลังเฝ้ารอ Picture-in-Picture...")
	a.startBtn.Disable()
	a.stopBtn.Enable()

	a.mu.Lock()
	a.routed = make(map[int]bool)
	a.sessionActive = false
	a.fallbackUsed = false
	a.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	go a.watchLoop(ctx)
}

func (a *recorderApp) stop() {
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	a.mu.Lock()
	for id := range a.routed {
		if err := moveSinkInput(id, "@DEFAULT_SINK@"); err != nil {
			a.appendLog("ย้าย stream #%d กลับไม่สำเร็จ: %v", id, err)
		} else {
			a.appendLog("ย้าย stream #%d กลับ default sink แล้ว", id)
		}
	}
	a.routed = make(map[int]bool)
	a.mu.Unlock()

	a.setStatus("สถานะ: หยุดแล้ว")
	a.startBtn.Enable()
	a.stopBtn.Disable()
}

func (a *recorderApp) watchLoop(ctx context.Context) {
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.tick()
		}
	}
}

func (a *recorderApp) tick() {
	pid, found, err := findPiPWindowPID()
	if err != nil {
		a.appendLog("เช็คหน้าต่าง PiP ผิดพลาด: %v", err)
		return
	}

	a.mu.Lock()
	wasActive := a.sessionActive
	a.mu.Unlock()

	if !found {
		// PiP ปิดไปแล้ว รีเซ็ตสถานะเซสชัน เผื่อรอบหน้าจะเปิดใหม่
		if wasActive {
			a.mu.Lock()
			a.sessionActive = false
			a.fallbackUsed = false
			a.mu.Unlock()
		}
		return
	}

	isRisingEdge := !wasActive
	if isRisingEdge {
		a.mu.Lock()
		a.sessionActive = true
		a.mu.Unlock()
	}

	inputs, err := listSinkInputs()
	if err != nil {
		a.appendLog("อ่าน audio stream ผิดพลาด: %v", err)
		return
	}

	// debug log ทุกครั้งที่เจอ PiP รอบใหม่ ช่วยไล่ดูว่า PID ที่ได้กับที่มีใน stream ตรงกันไหม
	if isRisingEdge {
		var ids []string
		for _, si := range inputs {
			ids = append(ids, fmt.Sprintf("#%d pid=%d app=%s", si.ID, si.ProcessID, si.AppName))
		}
		a.appendLog("[debug] เจอ PiP PID=%d | streams ตอนนี้: %s", pid, strings.Join(ids, ", "))
	}

	matched := false
	for _, si := range inputs {
		if si.ProcessID != pid {
			continue
		}
		matched = true
		a.mu.Lock()
		already := a.routed[si.ID]
		a.mu.Unlock()
		if already {
			continue
		}
		if err := moveSinkInput(si.ID, sinkName); err != nil {
			a.appendLog("ย้าย stream #%d ไม่สำเร็จ: %v", si.ID, err)
			continue
		}
		a.mu.Lock()
		a.routed[si.ID] = true
		a.mu.Unlock()
		a.appendLog("เจอ PiP (PID %d) -> ย้าย stream #%d (%s) เข้า %s แล้ว", pid, si.ID, si.AppName, sinkName)
	}

	if matched {
		return
	}

	// Fallback: ไม่เจอ process.id ตรงกันเป๊ะ (เช่น Firefox แยก process ต่อแท็บ)
	// ทำแค่ครั้งเดียวต่อการเปิด PiP หนึ่งรอบ ไม่งั้นจะสุ่มกวาดทุก stream ทีละตัวไปเรื่อยๆ
	a.mu.Lock()
	alreadyFellBack := a.fallbackUsed
	a.mu.Unlock()
	if alreadyFellBack {
		return
	}

	// เลือก stream ของ Firefox ที่ยังไม่ถูกย้าย โดยเอาตัวที่ id สูงสุด (สร้างล่าสุด)
	var fallback *SinkInput
	for i := range inputs {
		si := &inputs[i]
		if !strings.Contains(strings.ToLower(si.AppName), "firefox") {
			continue
		}
		a.mu.Lock()
		already := a.routed[si.ID]
		a.mu.Unlock()
		if already {
			continue
		}
		if fallback == nil || si.ID > fallback.ID {
			fallback = si
		}
	}

	a.mu.Lock()
	a.fallbackUsed = true
	a.mu.Unlock()

	if fallback == nil {
		return
	}
	if err := moveSinkInput(fallback.ID, sinkName); err != nil {
		a.appendLog("ย้าย stream fallback ไม่สำเร็จ: %v", err)
		return
	}
	a.mu.Lock()
	a.routed[fallback.ID] = true
	a.mu.Unlock()
	a.appendLog("ไม่พบ PID ตรงกัน ใช้ fallback (ครั้งเดียว): ย้าย stream #%d (%s) เข้า %s", fallback.ID, fallback.AppName, sinkName)
}

func main() {
	a := app.New()
	w := a.NewWindow("OBS PiP Router")
	w.Resize(fyne.NewSize(480, 360))

	ra := &recorderApp{win: w}
	ra.status = widget.NewLabel("สถานะ: ยังไม่เริ่ม")
	ra.logBox = widget.NewMultiLineEntry()
	ra.logBox.Wrapping = fyne.TextWrapWord
	ra.logBox.SetMinRowsVisible(10)

	ra.startBtn = widget.NewButton("เริ่มอัด", ra.start)
	ra.stopBtn = widget.NewButton("หยุด", ra.stop)
	ra.stopBtn.Disable()

	buttons := container.NewHBox(ra.startBtn, ra.stopBtn)
	content := container.NewBorder(
		container.NewVBox(ra.status, buttons),
		nil, nil, nil,
		container.NewScroll(ra.logBox),
	)
	w.SetContent(content)
	w.ShowAndRun()
}
