package compose

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// oemCopyCommand copies the stick's $OEM$\$$ folder onto the new Windows when
// Setup has not, so the first-boot script and everything it runs are there.
//
// Setup is documented to copy sources\$OEM$ by itself, and until Windows 11
// 24H2 it did. The installer in 24H2 and 25H2 skips it: an HP laptop imaged
// from a DSKY stick finished Setup, made the local account and signed in,
// then did nothing, because C:\Windows\Setup\Scripts\firstboot.cmd was never
// put there. Forum reports match ("Not requesting setupcomplete.cmd file
// migration" in setupact.log).
//
// It runs in the specialize pass, where the stick is still plugged in and has
// a drive letter, and before anything that needs the scripts. It copies nothing
// when Setup already did. RunSynchronous paths are limited to 259
// characters, which is why this is one terse line.
const oemCopyCommand = `cmd /c for %d in (D E F G H I J K L M N O P Q R S T U V W X Y Z) do @if exist %d:\sources\$OEM$\$$\Setup\Scripts\firstboot.cmd if not exist C:\Windows\Setup\Scripts\firstboot.cmd xcopy %d:\sources\$OEM$\$$ C:\Windows /e /i /h /y`

const oemCopyDescription = "DSKY: copy first-boot scripts from the stick if Setup did not"

var orderTag = regexp.MustCompile(`<Order>\s*(\d+)\s*</Order>`)

// withOEMCopy adds oemCopyCommand to a rendered answer file as the first
// specialize-pass command. The answer file may be a workspace's own template,
// so it is edited in place rather than regenerated: into an existing
// Microsoft-Windows-Deployment component (renumbering its commands, since
// Setup runs them by Order), or as a new component, or a new specialize
// section. The result must still parse.
func withOEMCopy(unattend string) (string, error) {
	return firstSpecializeCommand(unattend, oemCopyDescription, oemCopyCommand)
}

// firstSpecializeCommand adds a command to a rendered answer file as the first
// specialize-pass command, as withOEMCopy describes. Adding the same command
// twice changes nothing.
func firstSpecializeCommand(unattend, description, command string) (string, error) {
	if strings.Contains(unattend, description) {
		return unattend, nil
	}
	cmd := "\n        <RunSynchronousCommand wcm:action=\"add\" xmlns:wcm=\"http://schemas.microsoft.com/WMIConfig/2002/State\">" +
		"\n          <Order>1</Order>" +
		"\n          <Description>" + description + "</Description>" +
		"\n          <Path>" + xmlEscape(command) + "</Path>" +
		"\n        </RunSynchronousCommand>"
	component := "\n    <component name=\"Microsoft-Windows-Deployment\" processorArchitecture=\"amd64\"" +
		" publicKeyToken=\"31bf3856ad364e35\" language=\"neutral\" versionScope=\"nonSxS\">" +
		"\n      <RunSynchronous>" + cmd + "\n      </RunSynchronous>\n    </component>\n"

	out := unattend
	spec := regexp.MustCompile(`<settings\s+pass="specialize"\s*>`).FindStringIndex(out)
	if spec == nil {
		// No specialize pass at all: add one before the closing tag.
		end := strings.LastIndex(out, "</unattend>")
		if end < 0 {
			return "", errors.New("compose: autounattend.xml has no </unattend>")
		}
		out = out[:end] + "  <settings pass=\"specialize\">" + component + "  </settings>\n" + out[end:]
	} else {
		secEnd := strings.Index(out[spec[1]:], "</settings>")
		if secEnd < 0 {
			return "", errors.New("compose: autounattend.xml specialize section is not closed")
		}
		secEnd += spec[1]
		section := out[spec[1]:secEnd]
		dep := strings.Index(section, `name="Microsoft-Windows-Deployment"`)
		switch {
		case dep < 0:
			out = out[:secEnd] + component + "  " + out[secEnd:]
		default:
			compEnd := strings.Index(section[dep:], "</component>")
			if compEnd < 0 {
				return "", errors.New("compose: autounattend.xml Deployment component is not closed")
			}
			comp := section[dep : dep+compEnd]
			open := strings.Index(comp, "<RunSynchronous>")
			var newComp string
			if open < 0 {
				tagEnd := strings.Index(comp, ">")
				newComp = comp[:tagEnd+1] + "\n      <RunSynchronous>" + cmd + "\n      </RunSynchronous>" + comp[tagEnd+1:]
			} else {
				rest := orderTag.ReplaceAllStringFunc(comp[open:], func(m string) string {
					n, _ := strconv.Atoi(orderTag.FindStringSubmatch(m)[1])
					return fmt.Sprintf("<Order>%d</Order>", n+1)
				})
				newComp = comp[:open] + "<RunSynchronous>" + cmd + rest[len("<RunSynchronous>"):]
			}
			abs := spec[1] + dep
			out = out[:abs] + newComp + out[abs+compEnd:]
		}
	}
	dec := xml.NewDecoder(strings.NewReader(out))
	for {
		if _, err := dec.Token(); err == io.EOF {
			break
		} else if err != nil {
			return "", fmt.Errorf("compose: adding %q broke autounattend.xml: %w", description, err)
		}
	}
	return out, nil
}

func xmlEscape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}
