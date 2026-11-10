SHELL := /bin/bash
TARGET_ABI := arm64-v8a
ASSETS_DIR := app/src/main/assets
BUILD_DIR := build
DIST_DIR := $(BUILD_DIR)/dist
ROOTFS_7Z := $(ASSETS_DIR)/rootfs.7z
TEMP_ROOTFS := $(BUILD_DIR)/custom_rootfs
KEYSTORE := twoyi-release.jks
BASE_APK_URL := https://github.com/twoyi/twoyi/releases/download/0.5.4/twoyi_0.5.4-03211927-release.apk

.PHONY: all prepare rom native apk sign clean

all: prepare rom native apk sign

prepare:
	mkdir -p $(ASSETS_DIR) $(BUILD_DIR) $(DIST_DIR)
	@if [ ! -f "$(ROOTFS_7Z)" ]; then \
		echo "[*] Downloading base APK to extract rootfs..."; \
		wget -q "$(BASE_APK_URL)" -O $(BUILD_DIR)/base.apk; \
		7z e -y $(BUILD_DIR)/base.apk assets/rootfs.7z -o$(ASSETS_DIR); \
		rm -f $(BUILD_DIR)/base.apk; \
	fi

rom: prepare
	rm -rf $(TEMP_ROOTFS)
	mkdir -p $(TEMP_ROOTFS)
	@echo "[*] Unpacking rootfs..."
	-7z x -y $(ROOTFS_7Z) -o$(TEMP_ROOTFS) || [ $$? -le 2 ]
	@echo "[*] Normalizing file permissions..."
	chmod -R u+rw $(TEMP_ROOTFS)
	@echo "[*] Injecting su binary and in-guest updater..."
	mkdir -p $(TEMP_ROOTFS)/system/xbin $(TEMP_ROOTFS)/system/bin
	@echo -e '#!/system/bin/sh\nexport PATH=/system/xbin:/system/bin:\$$PATH\nexec /system/bin/sh "$$@"' > $(TEMP_ROOTFS)/system/xbin/su
	cp $(TEMP_ROOTFS)/system/xbin/su $(TEMP_ROOTFS)/system/bin/su
	chmod 4755 $(TEMP_ROOTFS)/system/xbin/su $(TEMP_ROOTFS)/system/bin/su
	@echo -e '#!/system/bin/sh\necho "Twoyi In-Guest Updater Ready"' > $(TEMP_ROOTFS)/system/xbin/twoyi-update
	chmod 0755 $(TEMP_ROOTFS)/system/xbin/twoyi-update
	@echo "[*] Patching build.prop..."
	@if [ -f "$(TEMP_ROOTFS)/system/build.prop" ]; then \
		sed -i 's/ro.secure=1/ro.secure=0/' $(TEMP_ROOTFS)/system/build.prop; \
		echo "ro.debuggable=1" >> $(TEMP_ROOTFS)/system/build.prop; \
		echo "persist.sys.root_access=3" >> $(TEMP_ROOTFS)/system/build.prop; \
	fi
	@echo "[*] Debloating unneeded packages..."
	rm -rf $(TEMP_ROOTFS)/system/priv-app/Velvet $(TEMP_ROOTFS)/system/app/YouTube
	@echo "[*] Repacking rootfs.7z..."
	rm -f $(ROOTFS_7Z)
	cd $(TEMP_ROOTFS) && (7z a -mx=9 ../../$(ROOTFS_7Z) . || [ $$? -le 2 ])
	cp $(ROOTFS_7Z) $(DIST_DIR)/rootfs-ota-v101.7z

native:
	chmod +x gradlew
	./gradlew cargoBuild

apk:
	chmod +x gradlew
	./gradlew assembleRelease

sign:
	@if [ ! -f "$(KEYSTORE)" ]; then \
		keytool -genkey -v -keystore $(KEYSTORE) -alias twoyikey -keyalg RSA -keysize 2048 -validity 10000 -storepass android -keypass android -dname "CN=TwoyiCustom, OU=Dev, O=Self, L=Perth, S=WA, C=AU"; \
	fi
	zipalign -v -p -f 4 app/build/outputs/apk/release/app-release-unsigned.apk $(DIST_DIR)/twoyi-aligned.apk
	apksigner sign --ks $(KEYSTORE) --ks-key-alias twoyikey --ks-pass pass:android --key-pass pass:android --out $(DIST_DIR)/twoyi-custom-final.apk $(DIST_DIR)/twoyi-aligned.apk
	apksigner verify --verbose $(DIST_DIR)/twoyi-custom-final.apk

clean:
	rm -rf $(BUILD_DIR) app/build
