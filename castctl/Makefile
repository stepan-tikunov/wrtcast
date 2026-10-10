# Host build. For OpenWrt use the package in openwrt/castctl.
# macOS: make MBEDTLS=$(brew --prefix mbedtls@3)

MBEDTLS ?=
CFLAGS ?= -Os -Wall -Wextra -Wno-unused-parameter
ifneq ($(MBEDTLS),)
CPPFLAGS += -I$(MBEDTLS)/include
LDFLAGS += -L$(MBEDTLS)/lib
endif
LDLIBS = -lmbedtls -lmbedx509 -lmbedcrypto

castctl: src/castctl.c
	$(CC) $(CPPFLAGS) $(CFLAGS) $(LDFLAGS) -o $@ $< $(LDLIBS)

clean:
	rm -f castctl

.PHONY: clean
