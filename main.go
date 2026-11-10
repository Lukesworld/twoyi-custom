package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	OfficialReleaseRootFS = "https://github.com/twoyi/twoyi/releases/download/0.5.4/twoyi_0.5.4-03211927-release.apk"
	BusyboxARM64URL       = "https://github.com/docker-library/busybox/raw/master/glibc/busybox.tar.gz" // Alternative: toybox/busybox static binary
	KeyAlias              = "twoyikey"
	KeyPass               = "android"
	KeystoreFile          = "twoyi-release.jks"
	TargetABI             = "arm64-v8a"
)

type OTAManifest struct {
	VersionCode int    `json:"version_code"`
	VersionName string `json:"version_name"`
	Author      string `json:"author"`
	ReleaseDate string `json:"release_date"`
	ArchiveName string `json:"archive_name"`
	SHA256      string `json:"sha256"`
	UpdateURL   string `json:"update_url"`
}

func checkTool(name string) {
	_, err := exec.LookPath(name)
	if err != nil {
		fmt.Printf("❌ Critical dependency missing: '%s' is not installed or not in PATH.\n", name)
		os.Exit(1)
	}
}

func runCmd(title string, dir string, env []string, command string, args ...string) {
	fmt.Printf("\n==> [RUNNING]: %s\n", title)
	cmd := exec.Command(command, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Run(); err != nil {
		fmt.Printf("❌ Pipeline failed at step: %s (%v)\n", title, err)
		os.Exit(1)
	}
	fmt.Printf("✔ [SUCCESS]: %s\n", title)
}

func downloadFile(url, destPath string) error {
	fmt.Printf("[*] Downloading %s to %s...\n", url, destPath)
	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()

	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}

	_, err = io.Copy(out, resp.Body)
	return err
}

func calculateSHA256(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// Injects the in-container update client so you can execute `twoyi-update` inside the guest shell
func injectInternalUpdater(systemDir string, otaManifestURL string) error {
	xbinDir := filepath.Join(systemDir, "xbin")
	os.MkdirAll(xbinDir, 0755)
	scriptPath := filepath.Join(xbinDir, "twoyi-update")

	scriptContent := fmt.Sprintf(`#!/system/bin/sh
# Twoyi In-Container System Updater
MANIFEST_URL="%s"
STORAGE_DEST="/sdcard/Download/latest_rootfs.7z"

echo "=========================================="
echo "    Twoyi Guest System Update Utility     "
echo "=========================================="

if [ "$(id -u)" != "0" ]; then
    echo "❌ Error: You must run twoyi-update as superuser (su)."
    exit 1
fi

case "$1" in
    check)
        echo "[*] Checking OTA manifest at $MANIFEST_URL..."
        wget -qO /tmp/manifest.json "$MANIFEST_URL" || busybox wget -qO /tmp/manifest.json "$MANIFEST_URL"
        if [ $? -ne 0 ]; then
            echo "❌ Failed to reach OTA server."
            exit 1
        fi
        echo "✔ Available Update Info:"
        cat /tmp/manifest.json
        ;;
    download)
        echo "[*] Fetching manifest..."
        wget -qO /tmp/manifest.json "$MANIFEST_URL" || busybox wget -qO /tmp/manifest.json "$MANIFEST_URL"
        URL=$(grep -o '"update_url": "[^"]*' /tmp/manifest.json | grep -o '[^"]*$')
        if [ -z "$URL" ]; then
            echo "❌ Could not parse update URL."
            exit 1
        fi
        echo "[*] Downloading update payload to $STORAGE_DEST..."
        wget -O "$STORAGE_DEST" "$URL" || busybox wget -O "$STORAGE_DEST" "$URL"
        echo "✔ Staged at $STORAGE_DEST."
        echo "💡 To apply: Twoyi Settings -> Replace ROM -> Select latest_rootfs.7z"
        ;;
    apply-local)
        PAYLOAD="$2"
        if [ ! -f "$PAYLOAD" ]; then
            echo "❌ Usage: twoyi-update apply-local /path/to/archive.7z"
            exit 1
        fi
        cp "$PAYLOAD" "$STORAGE_DEST"
        echo "✔ Copied to $STORAGE_DEST. Trigger 'Replace ROM' in Twoyi Settings."
        ;;
    *)
        echo "Usage: twoyi-update {check|download|apply-local <path>}"
        ;;
esac
`, otaManifestURL)

	return os.WriteFile(scriptPath, []byte(scriptContent), 0755)
}

// Injects a minimal su script wrapper if a binary su is not present, ensuring super admin shell access
func injectSuperuserSu(systemDir string) {
	xbin := filepath.Join(systemDir, "xbin")
	bin := filepath.Join(systemDir, "bin")
	os.MkdirAll(xbin, 0755)
	os.MkdirAll(bin, 0755)

	suPath := filepath.Join(xbin, "su")
	if _, err := os.Stat(suPath); errors.Is(err, os.ErrNotExist) {
		// Embed standalone root privilege escalation wrapper for container environment
		suScript := `#!/system/bin/sh
# Twoyi Super Admin Shell Entrypoint
export PATH=/system/xbin:/system/bin:/sbin:/vendor/bin
if [ -z "$1" ]; then
    exec /system/bin/sh
else
    exec /system/bin/sh -c "$@"
fi
`
		os.WriteFile(suPath, []byte(suScript), 04755)
		os.WriteFile(filepath.Join(bin, "su"), []byte(suScript), 04755)
	} else {
		os.Chmod(suPath, 04755)
		os.Chmod(filepath.Join(bin, "su"), 04755)
	}
}

func patchBuildProp(propPath string) error {
	content, err := os.ReadFile(propPath)
	if err != nil {
		return err
	}

	lines := strings.Split(string(content), "\n")
	var updatedLines []string

	tweaks := map[string]string{
		"ro.product.model":        "Pixel 8 Pro",
		"ro.product.brand":        "google",
		"ro.product.manufacturer": "Google",
		"debug.sf.hw":             "1",
		"debug.egl.hw":            "1",
		"persist.sys.ui.hw":       "true",
		"ro.debuggable":           "1",
		"ro.secure":               "0",
		"persist.sys.root_access": "3",
		"ro.adb.secure":           "0",
	}

	seen := make(map[string]bool)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		replaced := false
		for k, v := range tweaks {
			if strings.HasPrefix(trimmed, k+"=") {
				updatedLines = append(updatedLines, fmt.Sprintf("%s=%s", k, v))
				seen[k] = true
				replaced = true
				break
			}
		}
		if !replaced {
			updatedLines = append(updatedLines, line)
		}
	}

	for k, v := range tweaks {
		if !seen[k] {
			updatedLines = append(updatedLines, fmt.Sprintf("%s=%s", k, v))
		}
	}

	return os.WriteFile(propPath, []byte(strings.Join(updatedLines, "\n")), 0644)
}

func main() {
	ndkPathFlag := flag.String("ndk", os.Getenv("ANDROID_NDK_HOME"), "Path to Android NDK r22b")
	otaServerFlag := flag.String("ota-server", "http://127.0.0.1:8080/manifest.json", "Remote or local OTA URL")
	versionCodeFlag := flag.Int("version-code", 101, "Target internal ROM version code")
	versionNameFlag := flag.String("version-name", "0.8.1-custom-admin", "Target internal ROM version name")
	flag.Parse()

	fmt.Println("==========================================================")
	fmt.Println("   Twoyi End-to-End Orchestrator: ROM, Native & Signing   ")
	fmt.Println("==========================================================")

	// Validate host build tools
	checkTool("7z")
	checkTool("git")
	checkTool("keytool")
	checkTool("zipalign")
	checkTool("apksigner")

	// Ensure NDK is available
	ndkDir := *ndkPathFlag
	if ndkDir == "" {
		fmt.Println("❌ Error: ANDROID_NDK_HOME is not set. Pass -ndk=/path/to/ndk-r22b or export ANDROID_NDK_HOME.")
		os.Exit(1)
	}
	os.Setenv("ANDROID_NDK_HOME", ndkDir)

	// 1. Setup asset directories
	assetsDir := filepath.Join("app", "src", "main", "assets")
	os.MkdirAll(assetsDir, 0755)
	rootFsArchive := filepath.Join(assetsDir, "rootfs.7z")
	tempExtractDir := filepath.Join("build", "custom_rootfs")
	distDir := filepath.Join("build", "dist")
	os.MkdirAll(distDir, 0755)

	// 2. Fetch baseline rootfs if missing
	if _, err := os.Stat(rootFsArchive); errors.Is(err, os.ErrNotExist) {
		fmt.Printf("[!] %s not found. Downloading base package to extract rootfs...\n", rootFsArchive)
		tmpApk := filepath.Join("build", "twoyi-base.apk")
		os.MkdirAll("build", 0755)
		if err := downloadFile(OfficialReleaseRootFS, tmpApk); err != nil {
			fmt.Printf("❌ Failed downloading base APK: %v\n", err)
			os.Exit(1)
		}
		runCmd("Extract stock rootfs from official APK", "", nil, "7z", "e", "-y", tmpApk, "assets/rootfs.7z", "-o"+assetsDir)
		os.Remove(tmpApk)
	}

	// 3. Customize the ROM (root, update engine, build.prop, debloat)
	os.RemoveAll(tempExtractDir)
	os.MkdirAll(tempExtractDir, 0755)
	cmdUnpack := exec.Command("7z", "x", "-y", rootFsArchive, "-o"+tempExtractDir)
	cmdUnpack.Stdout = os.Stdout
	cmdUnpack.Stderr = os.Stderr
	if err := cmdUnpack.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
			fmt.Println("⚠ 7z reported relative symlink warnings (exit code 2), continuing safely...")
		} else {
			fmt.Printf("❌ Failed unpacking rootfs: %v\n", err)
			os.Exit(1)
		}
	}

	systemDir := filepath.Join(tempExtractDir, "system")

	// Debloat unnecessary apps to maximize performance
	os.RemoveAll(filepath.Join(systemDir, "priv-app", "Velvet"))
	os.RemoveAll(filepath.Join(systemDir, "app", "YouTube"))

	// Inject root and in-guest updater
	injectSuperuserSu(systemDir)
	if err := injectInternalUpdater(systemDir, *otaServerFlag); err != nil {
		fmt.Printf("❌ Failed injecting updater: %v\n", err)
		os.Exit(1)
	}

	// Tweak system build properties
	buildProp := filepath.Join(systemDir, "build.prop")
	if _, err := os.Stat(buildProp); err == nil {
		patchBuildProp(buildProp)
	}

	// Write ROM metadata
	romIni := fmt.Sprintf("[rom]\nauthor=CustomAdmin\ncode=%d\nversion=%s\ndesc=Twoyi Super Admin ROM with In-Guest OTA\n",
		*versionCodeFlag, *versionNameFlag)
	os.WriteFile(filepath.Join(tempExtractDir, "rom.ini"), []byte(romIni), 0644)

	// Repack rootfs.7z
	os.Remove(rootFsArchive)
	cmd7z := exec.Command("7z", "a", "-mx=9", filepath.Join("..", "..", "..", rootFsArchive), ".")
	cmd7z.Dir = tempExtractDir
	cmd7z.Stdout = os.Stdout
	cmd7z.Stderr = os.Stderr
	if err := cmd7z.Run(); err != nil {
		fmt.Printf("❌ Failed repacking rootfs.7z: %v\n", err)
		os.Exit(1)
	}

	// Generate and hash the standalone OTA payload
	otaArchive := filepath.Join(distDir, fmt.Sprintf("rootfs-ota-v%d.7z", *versionCodeFlag))
	exec.Command("cp", rootFsArchive, otaArchive).Run()
	sha, _ := calculateSHA256(otaArchive)
	manifest := OTAManifest{
		VersionCode: *versionCodeFlag,
		VersionName: *versionNameFlag,
		Author:      "CustomAdmin",
		ReleaseDate: time.Now().Format("2006-01-02"),
		ArchiveName: filepath.Base(otaArchive),
		SHA256:      sha,
		UpdateURL:   fmt.Sprintf("http://127.0.0.1:8080/%s", filepath.Base(otaArchive)),
	}
	manifestData, _ := json.MarshalIndent(manifest, "", "  ")
	os.WriteFile(filepath.Join(distDir, "manifest.json"), manifestData, 0644)
	fmt.Printf("✔ OTA update assets generated in %s\n", distDir)

	// 4. Build Native Rust Components & Android APK
	gradleCmd := "./gradlew"
	if runtime.GOOS == "windows" {
		gradleCmd = "gradlew.bat"
	}

	runCmd("Compile Native Rust Subsystems", "", nil, gradleCmd, "cargoBuild")
	runCmd("Assemble Release APK", "", nil, gradleCmd, "assembleRelease")

	// 5. Align and Sign
	unsignedAPK := filepath.Join("app", "build", "outputs", "apk", "release", "app-release-unsigned.apk")
	alignedAPK := filepath.Join(distDir, "twoyi-aligned.apk")
	finalAPK := filepath.Join(distDir, "twoyi-custom-final.apk")

	if _, err := os.Stat(KeystoreFile); errors.Is(err, os.ErrNotExist) {
		runCmd("Generate Keystore", "", nil, "keytool", "-genkey", "-v",
			"-keystore", KeystoreFile,
			"-alias", KeyAlias,
			"-keyalg", "RSA",
			"-keysize", "2048",
			"-validity", "10000",
			"-storepass", KeyPass,
			"-keypass", KeyPass,
			"-dname", "CN=TwoyiCustom, OU=Dev, O=Self, L=Perth, S=WA, C=AU")
	}

	os.Remove(alignedAPK)
	runCmd("ZipAlign APK", "", nil, "zipalign", "-v", "-p", "4", unsignedAPK, alignedAPK)

	os.Remove(finalAPK)
	runCmd("Sign APK with apksigner", "", nil, "apksigner", "sign",
		"--ks", KeystoreFile,
		"--ks-key-alias", KeyAlias,
		"--ks-pass", "pass:"+KeyPass,
		"--key-pass", "pass:"+KeyPass,
		"--out", finalAPK,
		alignedAPK)

	runCmd("Verify Signature", "", nil, "apksigner", "verify", "--verbose", finalAPK)

	fmt.Println("\n==========================================================")
	fmt.Printf("🎉 Build Complete and Verified!\n")
	fmt.Printf("   • Output APK:   %s\n", finalAPK)
	fmt.Printf("   • In-Guest OTA: %s\n", otaArchive)
	fmt.Printf("   • Manifest:     %s\n", filepath.Join(distDir, "manifest.json"))
	fmt.Println("==========================================================")
}
