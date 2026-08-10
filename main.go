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
	"sync"

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

// sinkExists ตรวจสอบว่า virtual sink มีอยู่แล้วหรือยัง
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
		// property ในหัวข้อ "Properties:" รูปแบบ key = "value"
		for _, pm := range propLineRe.FindAllStringSubmatch(block, -1) {
			si.Props[pm[1]] = pm[2]
		}
		// "Media Name:" อยู่นอกส่วน Properties แยกต่างหาก เก็บไว้เป็น media.name
		// สำรอง เผื่อ Properties ไม่มี media.name/media.title
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

// ---------- ส่วน GUI ----------

type recorderApp struct {
	win        fyne.Window
	status     *widget.Label
	sourcesBox *fyne.Container
	scanBtn    *widget.Button
	mu         sync.Mutex
}

func (a *recorderApp) setStatus(text string) {
	a.status.SetText(text)
}

// scan ดึงรายชื่อ audio stream ทั้งหมดตอนนี้ แล้วสร้างปุ่ม toggle ให้แต่ละตัว
func (a *recorderApp) scan() {
	if err := ensureSink(); err != nil {
		a.setStatus(fmt.Sprintf("สร้าง sink ไม่สำเร็จ: %v", err))
		return
	}
	obsIndex, err := getSinkIndex(sinkName)
	if err != nil {
		a.setStatus(fmt.Sprintf("หา sink ไม่เจอ: %v", err))
		return
	}
	inputs, err := listSinkInputs()
	if err != nil {
		a.setStatus(fmt.Sprintf("อ่าน audio stream ผิดพลาด: %v", err))
		return
	}

	var objs []fyne.CanvasObject
	if len(inputs) == 0 {
		objs = append(objs, widget.NewLabel("ไม่พบแหล่งเสียงที่กำลังเล่นอยู่ตอนนี้"))
	}
	for _, si := range inputs {
		si := si // capture ตัวแปรสำหรับ closure
		label := si.DisplayLabel()
		connected := si.SinkIndex == obsIndex
		prefix := "⬜ "
		importance := widget.MediumImportance
		if connected {
			prefix = "✅ "
			importance = widget.SuccessImportance
		}
		btnLabel := fmt.Sprintf("%s%s (#%d)", prefix, label, si.ID)

		btn := widget.NewButton(btnLabel, nil)
		btn.Importance = importance
		btn.OnTapped = func() {
			var target string
			if connected {
				target = "@DEFAULT_SINK@"
			} else {
				target = sinkName
			}
			if err := moveSinkInput(si.ID, target); err != nil {
				a.setStatus(fmt.Sprintf("ย้าย stream #%d ไม่สำเร็จ: %v", si.ID, err))
				return
			}
			a.scan() // สแกนใหม่เพื่ออัปเดตสถานะปุ่มทั้งหมดให้ตรงความจริง
		}
		objs = append(objs, btn)
	}

	a.sourcesBox.Objects = objs
	a.sourcesBox.Refresh()
	a.setStatus(fmt.Sprintf("สถานะ: เจอ %d แหล่งเสียง (สแกนล่าสุด)", len(inputs)))
}

func main() {
	a := app.New()
	w := a.NewWindow("OBS Audio Router")
	w.Resize(fyne.NewSize(480, 420))

	ra := &recorderApp{win: w}
	ra.status = widget.NewLabel("สถานะ: ยังไม่ได้สแกน")
	ra.sourcesBox = container.NewVBox()
	ra.scanBtn = widget.NewButton("สแกนแหล่งเสียง", ra.scan)

	content := container.NewBorder(
		container.NewVBox(ra.status, ra.scanBtn, widget.NewSeparator()),
		nil, nil, nil,
		container.NewVScroll(ra.sourcesBox),
	)
	w.SetContent(content)
	w.ShowAndRun()
}
