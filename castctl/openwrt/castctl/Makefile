include $(TOPDIR)/rules.mk

PKG_NAME:=castctl
PKG_VERSION:=0.1
PKG_RELEASE:=1
PKG_LICENSE:=MIT

include $(INCLUDE_DIR)/package.mk

define Package/castctl
  SECTION:=multimedia
  CATEGORY:=Multimedia
  TITLE:=Minimal Chromecast controller (play HLS, pause, seek, volume)
  DEPENDS:=+libmbedtls
endef

define Build/Prepare
	mkdir -p $(PKG_BUILD_DIR)
	$(CP) $(CURDIR)/../../src/castctl.c $(PKG_BUILD_DIR)/
endef

define Build/Compile
	$(TARGET_CC) $(TARGET_CPPFLAGS) $(TARGET_CFLAGS) $(TARGET_LDFLAGS) \
		-o $(PKG_BUILD_DIR)/castctl $(PKG_BUILD_DIR)/castctl.c \
		-lmbedtls -lmbedx509 -lmbedcrypto
endef

define Package/castctl/install
	$(INSTALL_DIR) $(1)/usr/bin
	$(INSTALL_BIN) $(PKG_BUILD_DIR)/castctl $(1)/usr/bin/
endef

$(eval $(call BuildPackage,castctl))
