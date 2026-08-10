// Copyright (c) 2026 Nawakarit
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License v3.0.
package main

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

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
	SinkIndex int               // index ของ sink ปลายทางที่ stream นี้กำลังต่ออยู่ตอนนี้
	Props     map[string]string // property ทั้งหมด เช่น application.name, media.title, application.process.id
}

// DisplayLabel รวม property ที่น่าจะบอกได้ว่าเป็นแหล่งเสียงไหน
// ลำดับความสำคัญ: media.title > media.name (ถ้า Firefox/แอปไม่ส่งชื่อแท็บมา
// จะเหลือแค่ "Playback" เฉยๆ ซึ่งเป็นข้อจำกัดของแอปนั้น ไม่ใช่บั๊กโปรแกรม)
func (si SinkInput) DisplayLabel() string {
	appName := si.Props["application.name"]
	if appName == "" {
		appName = "Unknown"
	}
	title := si.Props["media.title"]
	if title == "" {
		title = si.Props["media.name"]
	}

	label := appName
	if title != "" && title != appName {
		label = fmt.Sprintf("%s — %s", appName, title)
	}
	if pid := si.Props["application.process.id"]; pid != "" {
		label = fmt.Sprintf("%s [pid %s]", label, pid)
	}
	return label
}

func runCmd(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// getSinkIndex หา index ตัวเลขของ sink จากชื่อ (จาก `pactl list sinks short`)
func getSinkIndex(name string) (int, error) {
	out, err := runCmd("pactl", "list", "sinks", "short")
	if err != nil {
		return -1, err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[1] == name {
			idx, err := strconv.Atoi(fields[0])
			if err != nil {
				return -1, err
			}
			return idx, nil
		}
	}
	return -1, fmt.Errorf("ไม่พบ sink ชื่อ %s", name)
}

func sinkExists() bool {
	_, err := getSinkIndex(sinkName)
	return err == nil
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

var sinkInputHeaderRe = regexp.MustCompile(`(?m)^Sink Input #(\d+)`)
var sinkIndexRe = regexp.MustCompile(`(?m)^\s*Sink:\s*(\d+)`)
var mediaNameLineRe = regexp.MustCompile(`(?m)^\s*Media Name:\s*"([^"]*)"`)
var propLineRe = regexp.MustCompile(`(?m)^\s*([\w.]+)\s*=\s*"([^"]*)"`)

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
		si := SinkInput{ID: id, SinkIndex: -1, Props: map[string]string{}}
		if sm := sinkIndexRe.FindStringSubmatch(block); sm != nil {
			si.SinkIndex, _ = strconv.Atoi(sm[1])
		}
		for _, pm := range propLineRe.FindAllStringSubmatch(block, -1) {
			si.Props[pm[1]] = pm[2]
		}
		if mm := mediaNameLineRe.FindStringSubmatch(block); mm != nil {
			if _, ok := si.Props["media.name"]; !ok {
				si.Props["media.name"] = mm[1]
			}
		}
		result = append(result, si)
	}
	return result, nil
}

// moveSinkInput ย้าย audio stream ไปยัง sink ปลายทาง (ระบุด้วยชื่อ)
func moveSinkInput(id int, sink string) error {
	_, err := runCmd("pactl", "move-sink-input", strconv.Itoa(id), sink)
	return err
}

// รูปแบบบรรทัดของ `wmctrl -lx`:
// <window_id> <desktop> <WM_CLASS> <hostname> <title...>
var wmctrlLineRe = regexp.MustCompile(`^(\S+)\s+(-?\d+)\s+(\S+)\s+(\S+)\s+(.*)$`)

// findPiPWindowPID มองหาหน้าต่าง Picture-in-Picture ของ Firefox
// สังเกตจาก WM_CLASS ที่ขึ้นต้นด้วย "Toolkit" (คงที่ไม่ว่า UI จะภาษาอะไร)
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

// ---------- ส่วน GUI ----------

type recorderApp struct {
	win        fyne.Window
	status     *widget.Label
	sourcesBox *fyne.Container
}

func (a *recorderApp) setStatus(text string) {
	a.status.SetText(text)
}

// loadState สร้าง sink ถ้ายังไม่มี แล้วดึงข้อมูล obsIndex + stream ทั้งหมด
func (a *recorderApp) loadState() (int, []SinkInput, error) {
	if err := ensureSink(); err != nil {
		return -1, nil, fmt.Errorf("สร้าง sink ไม่สำเร็จ: %w", err)
	}
	obsIndex, err := getSinkIndex(sinkName)
	if err != nil {
		return -1, nil, fmt.Errorf("หา sink ไม่เจอ: %w", err)
	}
	inputs, err := listSinkInputs()
	if err != nil {
		return -1, nil, fmt.Errorf("อ่าน audio stream ผิดพลาด: %w", err)
	}
	return obsIndex, inputs, nil
}

// render สร้างปุ่ม toggle ให้แต่ละ stream ตามข้อมูลที่มี
func (a *recorderApp) render(obsIndex int, inputs []SinkInput) {
	var objs []fyne.CanvasObject
	if len(inputs) == 0 {
		objs = append(objs, widget.NewLabel("ไม่พบแหล่งเสียงที่กำลังเล่นอยู่ตอนนี้"))
	}
	for _, si := range inputs {
		si := si // capture ตัวแปรสำหรับ closure
		connected := si.SinkIndex == obsIndex
		prefix := "⬜ "
		importance := widget.MediumImportance
		if connected {
			prefix = "✅ "
			importance = widget.SuccessImportance
		}
		btnLabel := fmt.Sprintf("%s%s (#%d)", prefix, si.DisplayLabel(), si.ID)

		btn := widget.NewButton(btnLabel, nil)
		btn.Importance = importance
		btn.OnTapped = func() {
			target := sinkName
			if connected {
				target = "@DEFAULT_SINK@"
			}
			if err := moveSinkInput(si.ID, target); err != nil {
				a.setStatus(fmt.Sprintf("ย้าย stream #%d ไม่สำเร็จ: %v", si.ID, err))
				return
			}
			a.scan()
		}
		objs = append(objs, btn)
	}
	a.sourcesBox.Objects = objs
	a.sourcesBox.Refresh()
}

// scan ดึงรายชื่อ audio stream ทั้งหมดตอนนี้ แล้วสร้างปุ่ม toggle ให้แต่ละตัว
func (a *recorderApp) scan() {
	obsIndex, inputs, err := a.loadState()
	if err != nil {
		a.setStatus(err.Error())
		return
	}
	a.render(obsIndex, inputs)
	a.setStatus(fmt.Sprintf("สถานะ: เจอ %d แหล่งเสียง (สแกนล่าสุด)", len(inputs)))
}

// autoConnectFromPiP เช็คว่ามีหน้าต่าง Picture-in-Picture เปิดอยู่ไหม
// ถ้ามี จะตัดการเชื่อมต่อเดิมทั้งหมด แล้วเชื่อมเฉพาะ stream ของแท็บนั้น
// (แค่อันเดียว) เข้า OBS_Record ให้อัตโนมัติ
func (a *recorderApp) autoConnectFromPiP() {
	pid, found, err := findPiPWindowPID()
	if err != nil {
		a.setStatus(fmt.Sprintf("เช็คหน้าต่าง PiP ผิดพลาด: %v", err))
		return
	}
	if !found {
		a.setStatus("ไม่พบหน้าต่าง Picture-in-Picture ที่เปิดอยู่ตอนนี้")
		return
	}

	obsIndex, inputs, err := a.loadState()
	if err != nil {
		a.setStatus(err.Error())
		return
	}

	// ตัดการเชื่อมต่อเดิมทั้งหมดก่อน ให้เหลือแค่อันเดียวตามที่ต้องการ
	for _, si := range inputs {
		if si.SinkIndex == obsIndex {
			_ = moveSinkInput(si.ID, "@DEFAULT_SINK@")
		}
	}

	// ลอง match ด้วย PID ตรงๆ ก่อน (แม่นสุดถ้าตรง)
	pidStr := strconv.Itoa(pid)
	var target *SinkInput
	for i := range inputs {
		if inputs[i].Props["application.process.id"] == pidStr {
			target = &inputs[i]
			break
		}
	}

	usedFallback := false
	if target == nil {
		usedFallback = true
		for i := range inputs {
			if !strings.Contains(strings.ToLower(inputs[i].Props["application.name"]), "firefox") {
				continue
			}
			if target == nil || inputs[i].ID > target.ID {
				target = &inputs[i]
			}
		}
	}

	if target == nil {
		a.setStatus(fmt.Sprintf("เจอ PiP (pid=%d) แต่หา audio stream ของ Firefox ไม่เจอเลย", pid))
		obsIndex2, inputs2, _ := a.loadState()
		a.render(obsIndex2, inputs2)
		return
	}

	if err := moveSinkInput(target.ID, sinkName); err != nil {
		a.setStatus(fmt.Sprintf("เชื่อม stream #%d ไม่สำเร็จ: %v", target.ID, err))
		return
	}

	obsIndex2, inputs2, err := a.loadState()
	if err == nil {
		a.render(obsIndex2, inputs2)
	}

	if usedFallback {
		a.setStatus(fmt.Sprintf("⚠️ PID ไม่ตรงกัน (PiP pid=%d) ใช้ fallback เชื่อม stream ล่าสุดแทน: #%d — เช็คด้วยปุ่มด้านล่างว่าใช่ตัวที่ต้องการไหม", pid, target.ID))
	} else {
		a.setStatus(fmt.Sprintf("✅ เจอ PiP (pid=%d) ตรงกับ stream #%d เชื่อมเรียบร้อยแล้ว", pid, target.ID))
	}
}

func main() {
	a := app.New()
	w := a.NewWindow("OBS Audio Router")
	w.Resize(fyne.NewSize(520, 460))

	ra := &recorderApp{win: w}
	ra.status = widget.NewLabel("สถานะ: ยังไม่ได้สแกน")
	ra.sourcesBox = container.NewVBox()

	scanBtn := widget.NewButton("สแกนแหล่งเสียง", ra.scan)
	autoBtn := widget.NewButton("เชื่อมอัตโนมัติจาก PiP", ra.autoConnectFromPiP)
	autoBtn.Importance = widget.HighImportance

	content := container.NewBorder(
		container.NewVBox(ra.status, container.NewHBox(scanBtn, autoBtn), widget.NewSeparator()),
		nil, nil, nil,
		container.NewVScroll(ra.sourcesBox),
	)
	w.SetContent(content)
	w.ShowAndRun()
}
