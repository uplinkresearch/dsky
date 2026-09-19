//go:build windows

package agent

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The window, in plain Win32 calls.
//
// No toolkit: the agent is one static binary that Windows Setup copies onto a
// machine with nothing installed on it, and it stays that way. This is the
// only user interface DSKY puts on an imaged machine, and it is worth about
// three hundred lines to have the machine say what it is doing rather than
// look broken.

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	pRegisterClassExW = user32.NewProc("RegisterClassExW")
	pCreateWindowExW  = user32.NewProc("CreateWindowExW")
	pDefWindowProcW   = user32.NewProc("DefWindowProcW")
	pShowWindow       = user32.NewProc("ShowWindow")
	pUpdateWindow     = user32.NewProc("UpdateWindow")
	pGetMessageW      = user32.NewProc("GetMessageW")
	pTranslateMessage = user32.NewProc("TranslateMessage")
	pDispatchMessageW = user32.NewProc("DispatchMessageW")
	pPostQuitMessage  = user32.NewProc("PostQuitMessage")
	pDestroyWindow    = user32.NewProc("DestroyWindow")
	pInvalidateRect   = user32.NewProc("InvalidateRect")
	pBeginPaint       = user32.NewProc("BeginPaint")
	pEndPaint         = user32.NewProc("EndPaint")
	pFillRect         = user32.NewProc("FillRect")
	pDrawTextW        = user32.NewProc("DrawTextW")
	pGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	pPostMessageW     = user32.NewProc("PostMessageW")
	pSendMessageW     = user32.NewProc("SendMessageW")
	pLoadCursorW      = user32.NewProc("LoadCursorW")
	pSetForegroundWin = user32.NewProc("SetForegroundWindow")
	pGetClientRect    = user32.NewProc("GetClientRect")
	pGetForegroundWin = user32.NewProc("GetForegroundWindow")
	pIsWindowVisible  = user32.NewProc("IsWindowVisible")
	pAttachThreadInp  = user32.NewProc("AttachThreadInput")
	pGetWindowThread  = user32.NewProc("GetWindowThreadProcessId")
	pBringWindowTop   = user32.NewProc("BringWindowToTop")
	pSystemParamInfo  = user32.NewProc("SystemParametersInfoW")
	pMoveWindow       = user32.NewProc("MoveWindow")
	pSetWindowPos     = user32.NewProc("SetWindowPos")
	pSendInput        = user32.NewProc("SendInput")

	pCreateFontW      = gdi32.NewProc("CreateFontW")
	pCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
	pSelectObject     = gdi32.NewProc("SelectObject")
	pSetBkMode        = gdi32.NewProc("SetBkMode")
	pSetTextColor     = gdi32.NewProc("SetTextColor")
	pDeleteObject     = gdi32.NewProc("DeleteObject")
	pStretchDIBits    = gdi32.NewProc("StretchDIBits")

	pGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	pGetCurrentThread = kernel32.NewProc("GetCurrentThreadId")
	pOpenProcess      = kernel32.NewProc("OpenProcess")
	pQueryImageName   = kernel32.NewProc("QueryFullProcessImageNameW")
	pCloseHandle      = kernel32.NewProc("CloseHandle")
)

const (
	wsPopup            = 0x80000000
	wsOverlappedWindow = 0x00CF0000
	wsVisible          = 0x10000000
	wsChild            = 0x40000000
	wsExTopmost        = 0x00000008
	wsExNoActive       = 0x08000000
	bsPushButton       = 0x00000000
	swShow             = 5
	swHide             = 0
	wmDestroy          = 0x0002
	wmPaint            = 0x000F
	wmClose            = 0x0010
	wmCommand          = 0x0111
	wmTimer            = 0x0113
	wmKeyDown          = 0x0100
	wmSetFont          = 0x0030
	wmApp              = 0x8000
	vkEscape           = 0x1B
	smCXScreen         = 0
	smCYScreen         = 1
	dtLeft             = 0x0000
	dtWordBreak        = 0x0010
	dtNoPrefix         = 0x0800
	dtCalcRect         = 0x0400
	transparent        = 1
	idcArrow           = 32512
	firstButtonID      = 1000
	hwndTopmost        = ^uintptr(0) // (HWND)-1
	swpNoSize          = 0x0001
	swpNoMove          = 0x0002
	swpNoActivate      = 0x0010

	spiSetForegroundLockTimeout = 0x2001
	spifSendChange              = 0x0002
)

type rect struct{ left, top, right, bottom int32 }

type wndclassexw struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   windows.Handle
	icon       windows.Handle
	cursor     windows.Handle
	background windows.Handle
	menuName   *uint16
	className  *uint16
	iconSm     windows.Handle
}

type msgw struct {
	hwnd    windows.HWND
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
}

type bitmapInfoHeader struct {
	size          uint32
	width         int32
	height        int32
	planes        uint16
	bitCount      uint16
	compression   uint32
	sizeImage     uint32
	xPelsPerMeter int32
	yPelsPerMeter int32
	clrUsed       uint32
	clrImportant  uint32
}

type paintstruct struct {
	hdc         windows.Handle
	erase       int32
	paint       rect
	restore     int32
	incUpdate   int32
	rgbReserved [32]byte
}

// win32Window is the window itself. There is only ever one, so the window
// procedure reaches it through a package variable.
type win32Window struct {
	hwnd    windows.HWND
	screen  *screen
	buttons []windows.HWND

	big, mid, small windows.Handle // fonts
	bg              windows.Handle // background brush

	// The wordmark, decoded once into the rows GDI wants: top-down BGRA,
	// already composited over the background so nothing has to blend.
	logo  []byte
	logoW int32
	logoH int32

	ready chan error
	once  sync.Once
}

var theWindow *win32Window

// The palette the portal uses, in GDI's 0x00BBGGRR order: --bg #191e20,
// --text #eee9da, --dim #b6b8ac, --ok and --accent #a9e6a5, --err #f07d70. A
// machine being set up should look like the tool that is setting it up.
//
// These were the cyan-on-near-black set the portal had before it became a
// piece of ground support hardware, and they stayed behind when it changed.
// Nobody noticed, because the only screen they draw is on somebody else's
// machine during a first boot — which is the screen least able to afford
// looking like it came from a different program.
//
// --ok and --accent are the same phosphor now. They were distinct when green
// meant success and cyan meant brand; here the brand is the green.
const (
	colBG      = 0x00201E19 // #191e20
	colHeading = 0x00DAE9EE // #eee9da
	colBody    = 0x00DAE9EE // #eee9da
	colDim     = 0x00ACB8B6 // #b6b8ac
	colOK      = 0x00A5E6A9 // #a9e6a5
	colProblem = 0x00707DF0 // #f07d70
	colAccent  = 0x00A5E6A9 // #a9e6a5
)

// newWindow starts the window on a thread of its own and waits to hear
// whether it came up. A machine with no desktop -- which is not how the agent
// runs, but may be how somebody runs it by hand -- gets no window and no
// error that stops the work.
func newWindow(s *screen) (window, error) {
	w := &win32Window{screen: s, ready: make(chan error, 1)}
	go w.loop()
	select {
	case err := <-w.ready:
		if err != nil {
			return nil, err
		}
		return w, nil
	case <-time.After(20 * time.Second):
		return nil, errors.New("the status window did not open")
	}
}

// loop owns the window: it is created, drawn and destroyed on this one thread,
// which is what Windows requires of a message loop.
func (w *win32Window) loop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := w.create(); err != nil {
		w.ready <- err
		return
	}
	theWindow = w
	w.ready <- nil

	var m msgw
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	w.screen.dismissed()
}

func (w *win32Window) create() error {
	inst, _, _ := pGetModuleHandleW.Call(0)
	class := windows.StringToUTF16Ptr("DSKYStatus")
	cursor, _, _ := pLoadCursorW.Call(0, uintptr(idcArrow))
	brush, _, _ := pCreateSolidBrush.Call(colBG)
	w.bg = windows.Handle(brush)

	wc := wndclassexw{
		size:       uint32(unsafe.Sizeof(wndclassexw{})),
		wndProc:    syscall.NewCallback(wndProc),
		instance:   windows.Handle(inst),
		cursor:     windows.Handle(cursor),
		background: w.bg,
		className:  class,
	}
	if r, _, err := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return err
	}
	cx, _, _ := pGetSystemMetrics.Call(smCXScreen)
	cy, _, _ := pGetSystemMetrics.Call(smCYScreen)
	if cx == 0 || cy == 0 {
		return errors.New("no screen to draw on")
	}
	// A first boot takes the whole screen; a payload on somebody's machine
	// gets an ordinary window in the middle of it.
	exStyle, style := uintptr(wsExTopmost), uintptr(wsPopup|wsVisible)
	x, y, width, height := uintptr(0), uintptr(0), cx, cy
	if !w.screen.takeover {
		exStyle, style = 0, wsOverlappedWindow|wsVisible
		width, height = min(cx, 1100), min(cy, 760)
		x, y = (cx-width)/2, (cy-height)/2
	}
	hwnd, _, err := pCreateWindowExW.Call(
		exStyle,
		uintptr(unsafe.Pointer(class)),
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("DSKY"))),
		style,
		x, y, width, height,
		0, 0, uintptr(inst), 0)
	if hwnd == 0 {
		return err
	}
	w.hwnd = windows.HWND(hwnd)
	w.loadLogo()
	w.big = w.font(-44, 600)
	w.mid = w.font(-22, 400)
	w.small = w.font(-17, 400)

	// Windows makes a window wait before it may take the foreground. On a
	// machine being provisioned there is nothing to protect: this window is
	// the only thing that should be in front. On a machine somebody is
	// using, that setting is theirs, and it is left alone.
	if w.screen.takeover {
		pSystemParamInfo.Call(spiSetForegroundLockTimeout, 0, 0, spifSendChange)
	}

	pShowWindow.Call(hwnd, swShow)
	pUpdateWindow.Call(hwnd)
	pSetForegroundWin.Call(hwnd)
	// A second hand, so "installing drivers" shows how long it has been
	// installing drivers rather than looking stuck.
	user32.NewProc("SetTimer").Call(hwnd, 1, 1000, 0)
	return nil
}

// bgColour is colBG as Go sees it. GDI stores a COLORREF as 0x00BBGGRR, so
// the bytes come out in the opposite order to the way the constant reads.
func bgColour() color.RGBA {
	return color.RGBA{
		R: uint8(colBG & 0xff),
		G: uint8((colBG >> 8) & 0xff),
		B: uint8((colBG >> 16) & 0xff),
		A: 0xff,
	}
}

// loadLogo decodes the wordmark over the window's background colour. A window
// with no logo is a window with no logo: nothing here can stop a machine being
// provisioned.
func (w *win32Window) loadLogo() {
	img, err := png.Decode(bytes.NewReader(logoPNG))
	if err != nil {
		return
	}
	b := img.Bounds()
	dst := image.NewRGBA(b)
	// The same colour the window is painted with, written from colBG rather
	// than repeated by hand: the wordmark has soft edges, and flattening it
	// over anything else leaves a rectangle of the wrong dark around it.
	draw.Draw(dst, b, &image.Uniform{bgColour()}, image.Point{}, draw.Src)
	draw.Draw(dst, b, img, b.Min, draw.Over)

	w.logoW, w.logoH = int32(b.Dx()), int32(b.Dy())
	w.logo = make([]byte, 0, b.Dx()*b.Dy()*4)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := dst.RGBAAt(x, y)
			w.logo = append(w.logo, c.B, c.G, c.R, 0xff) // GDI wants BGRA
		}
	}
}

// drawLogo puts the wordmark at the top of the window and reports how much
// room it took.
func (w *win32Window) drawLogo(hdc uintptr, x, y int32) int32 {
	if len(w.logo) == 0 {
		return 0
	}
	hdr := bitmapInfoHeader{
		size:  uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		width: w.logoW, height: -w.logoH, // negative: rows top-down
		planes: 1, bitCount: 32, compression: 0,
	}
	pStretchDIBits.Call(hdc,
		uintptr(x), uintptr(y), uintptr(w.logoW), uintptr(w.logoH),
		0, 0, uintptr(w.logoW), uintptr(w.logoH),
		uintptr(unsafe.Pointer(&w.logo[0])), uintptr(unsafe.Pointer(&hdr)),
		0 /*DIB_RGB_COLORS*/, 0x00CC0020 /*SRCCOPY*/)
	return w.logoH
}

func (w *win32Window) font(height int32, weight int32) windows.Handle {
	h, _, _ := pCreateFontW.Call(
		uintptr(height), 0, 0, 0, uintptr(weight), 0, 0, 0,
		1 /*DEFAULT_CHARSET*/, 0, 0, 4 /*CLEARTYPE_QUALITY*/, 0,
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("Segoe UI"))))
	return windows.Handle(h)
}

// refresh is called from the agent's own goroutines: it only asks the window
// thread to repaint, and never touches the window itself.
func (w *win32Window) refresh() {
	if w == nil || w.hwnd == 0 {
		return
	}
	pPostMessageW.Call(uintptr(w.hwnd), wmApp, 0, 0)
}

func (w *win32Window) close() {
	if w == nil || w.hwnd == 0 {
		return
	}
	w.once.Do(func() { pPostMessageW.Call(uintptr(w.hwnd), wmClose, 0, 0) })
}

func wndProc(hwnd windows.HWND, message uint32, wParam, lParam uintptr) uintptr {
	w := theWindow
	switch message {
	case wmApp, wmTimer:
		if w != nil {
			w.syncButtons()
			if w.screen.takeover {
				w.keepInFront()
			}
		}
		pInvalidateRect.Call(uintptr(hwnd), 0, 1)
		return 0
	case wmPaint:
		if w != nil {
			w.paint()
		}
		return 0
	case wmCommand:
		id := int(wParam & 0xFFFF)
		if w != nil && id >= firstButtonID {
			select {
			case w.screen.clicked <- id - firstButtonID:
			default:
			}
		}
		return 0
	case wmKeyDown:
		// Esc puts the window out of the way. It never stops the work --
		// there is nothing here that should be cancellable by a keypress on
		// a machine being built.
		if wParam == vkEscape {
			pShowWindow.Call(uintptr(hwnd), swHide)
		}
		return 0
	case wmClose:
		pDestroyWindow.Call(uintptr(hwnd))
		return 0
	case wmDestroy:
		pPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(uintptr(hwnd), uintptr(message), wParam, lParam)
	return r
}

// keepInFront puts the window back in front when something else has taken
// over, once a second.
//
// Watched on an EliteBook: the Start menu was open across the status window
// for the whole build, because Windows opens it at first sign-in and the shell
// draws above even a topmost window. Taking the foreground is what closes it --
// Start closes the moment it loses focus -- so that is what this does.
//
// Not when the window is hidden. Esc hides it, and somebody who has hidden the
// window has said they want the machine to themselves; fighting them for the
// foreground every second would be the rudest thing this program could do.
func (w *win32Window) keepInFront() {
	if visible, _, _ := pIsWindowVisible.Call(uintptr(w.hwnd)); visible == 0 {
		return
	}
	fg, _, _ := pGetForegroundWin.Call()
	if fg == 0 || windows.HWND(fg) == w.hwnd {
		return
	}
	// Windows 11's Start menu is its own shell process, drawn above even a
	// topmost window, and taking the foreground away from it does not close
	// it: two releases tried exactly that on an HP EliteBook and it sat over
	// the window for the whole build both times. Escape closes it, as it does
	// by hand. Only when the Start menu or its search box has the foreground:
	// an Escape that reached this window instead would hide it.
	if shellFlyout(fg) {
		pressEscape()
	}
	pSetWindowPos.Call(uintptr(w.hwnd), hwndTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate)

	// Windows refuses SetForegroundWindow to a process that does not already
	// own the foreground, silently, which is why asking politely once a second
	// left the Start menu sitting over the window for a whole build. A thread
	// attached to the one that does own it is allowed to ask on its behalf:
	// the documented way in, and what every launcher and installer does.
	mine, _, _ := pGetCurrentThread.Call()
	theirs, _, _ := pGetWindowThread.Call(fg, 0)
	if theirs != 0 && theirs != mine {
		pAttachThreadInp.Call(mine, theirs, 1)
		pBringWindowTop.Call(uintptr(w.hwnd))
		pSetForegroundWin.Call(uintptr(w.hwnd))
		pAttachThreadInp.Call(mine, theirs, 0)
		return
	}
	pBringWindowTop.Call(uintptr(w.hwnd))
	pSetForegroundWin.Call(uintptr(w.hwnd))
}

// shellFlyout reports whether a window belongs to the Start menu or the search
// box that opens from it, by the program that owns it. Their window classes
// are shared with every other modern app; the process is what is specific.
func shellFlyout(hwnd uintptr) bool {
	var pid uint32
	pGetWindowThread.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return false
	}
	const processQueryLimitedInformation = 0x1000
	h, _, _ := pOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		return false
	}
	defer pCloseHandle.Call(h)
	buf := make([]uint16, 520)
	n := uint32(len(buf))
	if ok, _, _ := pQueryImageName.Call(h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); ok == 0 {
		return false
	}
	return isShellFlyoutImage(windows.UTF16ToString(buf[:n]))
}

// keyInput is Windows' INPUT carrying a KEYBDINPUT. Its size is checked in a
// test: SendInput refuses a structure of the wrong size by returning 0, which
// is to say it fails without a word, and that is the failure this whole
// function exists to get past.
type keyInput struct {
	typ       uint32
	_         uint32 // alignment: the union starts on an 8-byte boundary
	vk        uint16
	scan      uint16
	flags     uint32
	time      uint32
	extraInfo uintptr
	_         [8]byte // the union is as large as MOUSEINPUT, its largest member
}

// pressEscape sends one Escape keystroke to whatever has the foreground.
func pressEscape() {
	const inputKeyboard, keyUp = 1, 0x0002
	down := keyInput{typ: inputKeyboard, vk: vkEscape}
	up := keyInput{typ: inputKeyboard, vk: vkEscape, flags: keyUp}
	inputs := []keyInput{down, up}
	pSendInput.Call(uintptr(len(inputs)), uintptr(unsafe.Pointer(&inputs[0])), unsafe.Sizeof(inputs[0]))
}

// syncButtons makes the real buttons match what the screen is asking for.
func (w *win32Window) syncButtons() {
	w.screen.mu.Lock()
	want := append([]string(nil), w.screen.buttons...)
	w.screen.mu.Unlock()

	if len(want) == len(w.buttons) {
		return
	}
	for _, b := range w.buttons {
		pDestroyWindow.Call(uintptr(b))
	}
	w.buttons = nil

	var rc rect
	pGetClientRect.Call(uintptr(w.hwnd), uintptr(unsafe.Pointer(&rc)))
	const bw, bh, gap = 260, 56, 24
	x := int32(80)
	y := rc.bottom - bh - 80
	inst, _, _ := pGetModuleHandleW.Call(0)
	for i, label := range want {
		h, _, _ := pCreateWindowExW.Call(0,
			uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("BUTTON"))),
			uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(label))),
			wsChild|wsVisible|bsPushButton,
			uintptr(x+int32(i)*(bw+gap)), uintptr(y), bw, bh,
			uintptr(w.hwnd), uintptr(firstButtonID+i), uintptr(inst), 0)
		if h == 0 {
			continue
		}
		pSendMessageW.Call(h, wmSetFont, uintptr(w.mid), 1)
		w.buttons = append(w.buttons, windows.HWND(h))
	}
}

// paint draws the whole screen every time. There is nothing here worth the
// complication of drawing only what changed.
func (w *win32Window) paint() {
	var ps paintstruct
	hdc, _, _ := pBeginPaint.Call(uintptr(w.hwnd), uintptr(unsafe.Pointer(&ps)))
	defer pEndPaint.Call(uintptr(w.hwnd), uintptr(unsafe.Pointer(&ps)))

	var rc rect
	pGetClientRect.Call(uintptr(w.hwnd), uintptr(unsafe.Pointer(&rc)))
	pFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), uintptr(w.bg))
	pSetBkMode.Call(hdc, transparent)

	s := w.screen
	s.mu.Lock()
	heading, sub, machine := s.heading, s.sub, s.machine
	steps := append([]stepLine(nil), s.steps...)
	note := s.note
	summary := append([]string(nil), s.summary...)
	finished := s.finished
	s.mu.Unlock()

	const left = 80
	y := int32(56)
	if h := w.drawLogo(hdc, left, y); h > 0 {
		y += h + 40
	} else {
		y += 34
	}

	// Each line is measured before it is drawn, and the next starts below
	// however many lines it wrapped to. Advancing by a fixed height drew a
	// long failure -- a registry path and "Access is denied." -- over the
	// line after it, on the one screen that most needs to be readable.
	draw := func(text string, font windows.Handle, colour uintptr, height int32) {
		if text == "" {
			y += height
			return
		}
		pSelectObject.Call(hdc, uintptr(font))
		pSetTextColor.Call(hdc, colour)
		txt := windows.StringToUTF16Ptr(text)
		measure := rect{left, y, rc.right - left, y}
		pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(txt)), ^uintptr(0),
			uintptr(unsafe.Pointer(&measure)), dtLeft|dtWordBreak|dtNoPrefix|dtCalcRect)
		r := rect{left, y, rc.right - left, measure.bottom}
		pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(txt)), ^uintptr(0),
			uintptr(unsafe.Pointer(&r)), dtLeft|dtWordBreak|dtNoPrefix)
		used := measure.bottom - measure.top
		gap := height - lineHeight(height)
		if used+gap > height {
			y += used + gap
		} else {
			y += height
		}
	}

	draw(heading, w.big, colHeading, 70)
	draw(sub, w.mid, colDim, 56)

	if !finished && machine != "" {
		draw(machine, w.small, colDim, 44)
	}

	for _, st := range steps {
		mark, colour := "  ", uintptr(colBody)
		switch st.State {
		case stepDone:
			mark, colour = "OK  ", colOK
		case stepProblem:
			mark, colour = "!   ", colProblem
		case stepDoing:
			mark = "... "
		}
		line := mark + stepTitle(st.Name)
		if st.State == stepDoing {
			line += "  (" + shortDur(time.Since(st.Started)) + ")"
		}
		if st.Detail != "" {
			line += " - " + st.Detail
		}
		draw(line, w.mid, colour, 40)
	}

	for _, l := range summary {
		draw(l, w.mid, colBody, 40)
	}

	if note != "" {
		y += 20
		draw(note, w.mid, colHeading, 46)
	}

	// The way out, said quietly, at the bottom.
	pSelectObject.Call(hdc, uintptr(w.small))
	pSetTextColor.Call(hdc, colDim)
	hint := rect{left, rc.bottom - 40, rc.right - left, rc.bottom - 10}
	hintText := "Press Esc to hide this window. The work carries on either way."
	if finished {
		// There is no work left to carry on with.
		hintText = "Press Finish, or Esc, to close this."
	}
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(hintText))),
		^uintptr(0), uintptr(unsafe.Pointer(&hint)), dtLeft|dtNoPrefix)
}

// stepTitle is the step's name in the words somebody at the machine would use.
func stepTitle(step string) string {
	switch step {
	case stepDrivers:
		return "Drivers"
	case stepDebloat:
		return "Removing preinstalled extras"
	case stepApps:
		return "Installing programs"
	default:
		return step
	}
}

// lineHeight is roughly one line of text for a row of the given spacing, so
// the gap under a wrapped line matches the gap under an unwrapped one.
func lineHeight(rowSpacing int32) int32 { return rowSpacing * 3 / 4 }
