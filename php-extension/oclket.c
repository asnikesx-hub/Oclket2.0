#include "php.h"
#include "ext/standard/info.h"
#include "Zend/zend_exceptions.h"

#include <errno.h>
#include <fcntl.h>
#include <stddef.h>
#include <stdint.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <sys/types.h>
#include <sys/uio.h>
#include <sys/un.h>
#include <unistd.h>

#define PHP_OCLKET_VERSION "0.1.0"
#define OCLKET_MAX_PAYLOAD (128U * 1024U * 1024U)
#define OCLKET_IOV_MAX 6
#define OCLKET_ERROR_BUFFER_SIZE 512
#define OCLKET_IO_TIMEOUT_SECONDS 5

#define OCLKET_L_GET 0x434D4401U
#define OCLKET_L_SET 0x434D4402U
#define OCLKET_L_DEL 0x434D4403U

#define OCLKET_A_OK  0x434D4430U
#define OCLKET_A_ERR 0x434D4431U
#define OCLKET_A_ROW 0x434D4432U
#define OCLKET_A_EMP 0x434D4433U

ZEND_BEGIN_MODULE_GLOBALS(oclket)
	char *socket_path;
	int socket_fd;
	pid_t socket_pid;
	size_t connected_path_len;
	char connected_path[sizeof(((struct sockaddr_un *) 0)->sun_path)];
ZEND_END_MODULE_GLOBALS(oclket)

ZEND_DECLARE_MODULE_GLOBALS(oclket)
#define OCLKET_G(v) ZEND_MODULE_GLOBALS_ACCESSOR(oclket, v)

static void oclket_close_connection(void)
{
	if (OCLKET_G(socket_fd) >= 0) {
		close(OCLKET_G(socket_fd));
	}
	OCLKET_G(socket_fd) = -1;
	OCLKET_G(socket_pid) = 0;
	OCLKET_G(connected_path_len) = 0;
}

static PHP_GINIT_FUNCTION(oclket)
{
#if (defined(COMPILE_DL_OCLKET) || defined(COMPILE_DL_EXT) || defined(ZEND_COMPILE_DL_EXT)) && defined(ZTS)
	ZEND_TSRMLS_CACHE_UPDATE();
#endif
	oclket_globals->socket_fd = -1;
	oclket_globals->socket_pid = 0;
	oclket_globals->connected_path_len = 0;
	oclket_globals->connected_path[0] = '\0';
}

static PHP_GSHUTDOWN_FUNCTION(oclket)
{
	if (oclket_globals->socket_fd >= 0) {
		close(oclket_globals->socket_fd);
	}
}

PHP_INI_BEGIN()
	STD_PHP_INI_ENTRY(
		"oclket.socket_path",
		"/tmp/oclket.sock",
		PHP_INI_ALL,
		OnUpdateString,
		socket_path,
		zend_oclket_globals,
		oclket_globals
	)
PHP_INI_END()

static void oclket_throw_socket_error(const char *operation)
{
	int saved_errno = errno;
	oclket_close_connection();
	zend_throw_exception_ex(
		zend_ce_exception,
		0,
		"oclket %s failed: %s",
		operation,
		strerror(saved_errno)
	);
}

static zend_bool oclket_ensure_connection(void)
{
	const char *path = OCLKET_G(socket_path);
	size_t path_len;
	struct sockaddr_un address;
	struct timeval timeout;
	int fd;

	if (path == NULL || path[0] == '\0') {
		zend_throw_exception(zend_ce_exception, "oclket socket path is empty", 0);
		return 0;
	}

	path_len = strlen(path);
	if (path_len >= sizeof(address.sun_path)) {
		zend_throw_exception(zend_ce_exception, "oclket socket path is too long", 0);
		return 0;
	}

	if (OCLKET_G(socket_fd) >= 0 &&
		(OCLKET_G(socket_pid) != getpid() ||
		 OCLKET_G(connected_path_len) != path_len ||
		 memcmp(OCLKET_G(connected_path), path, path_len) != 0)) {
		oclket_close_connection();
	}

	if (OCLKET_G(socket_fd) >= 0) {
		return 1;
	}

	fd = socket(AF_UNIX, SOCK_STREAM, 0);
	if (fd < 0) {
		zend_throw_exception_ex(
			zend_ce_exception,
			0,
			"oclket socket creation failed: %s",
			strerror(errno)
		);
		return 0;
	}

	if (fcntl(fd, F_SETFD, FD_CLOEXEC) < 0) {
		int saved_errno = errno;
		close(fd);
		zend_throw_exception_ex(
			zend_ce_exception,
			0,
			"oclket setting close-on-exec failed: %s",
			strerror(saved_errno)
		);
		return 0;
	}

	timeout.tv_sec = OCLKET_IO_TIMEOUT_SECONDS;
	timeout.tv_usec = 0;
	if (setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout)) < 0 ||
		setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout)) < 0) {
		int saved_errno = errno;
		close(fd);
		zend_throw_exception_ex(
			zend_ce_exception,
			0,
			"oclket configuring socket timeout failed: %s",
			strerror(saved_errno)
		);
		return 0;
	}

	memset(&address, 0, sizeof(address));
	address.sun_family = AF_UNIX;
	memcpy(address.sun_path, path, path_len + 1);
	if (connect(
			fd,
			(struct sockaddr *) &address,
			(socklen_t) (offsetof(struct sockaddr_un, sun_path) + path_len + 1)
		) < 0) {
		int saved_errno = errno;
		close(fd);
		zend_throw_exception_ex(
			zend_ce_exception,
			0,
			"oclket connecting to %s failed: %s",
			path,
			strerror(saved_errno)
		);
		return 0;
	}

	OCLKET_G(socket_fd) = fd;
	OCLKET_G(socket_pid) = getpid();
	OCLKET_G(connected_path_len) = path_len;
	memcpy(OCLKET_G(connected_path), path, path_len + 1);
	return 1;
}

static void oclket_put_u32(uint8_t *dst, uint32_t value)
{
	dst[0] = (uint8_t) (value >> 24);
	dst[1] = (uint8_t) (value >> 16);
	dst[2] = (uint8_t) (value >> 8);
	dst[3] = (uint8_t) value;
}

static void oclket_put_u64(uint8_t *dst, uint64_t value)
{
	dst[0] = (uint8_t) (value >> 56);
	dst[1] = (uint8_t) (value >> 48);
	dst[2] = (uint8_t) (value >> 40);
	dst[3] = (uint8_t) (value >> 32);
	dst[4] = (uint8_t) (value >> 24);
	dst[5] = (uint8_t) (value >> 16);
	dst[6] = (uint8_t) (value >> 8);
	dst[7] = (uint8_t) value;
}

static uint32_t oclket_get_u32(const uint8_t *src)
{
	return ((uint32_t) src[0] << 24) |
		((uint32_t) src[1] << 16) |
		((uint32_t) src[2] << 8) |
		(uint32_t) src[3];
}

static zend_bool oclket_write_iov(const struct iovec *iov, int iov_count)
{
	struct iovec pending[OCLKET_IOV_MAX];
	int index = 0;

	if (iov_count < 1 || iov_count > OCLKET_IOV_MAX) {
		errno = EINVAL;
		oclket_throw_socket_error("write");
		return 0;
	}
	memcpy(pending, iov, (size_t) iov_count * sizeof(*iov));

	while (index < iov_count) {
		struct msghdr message;
		ssize_t written;
		size_t remaining;

		while (index < iov_count && pending[index].iov_len == 0) {
			index++;
		}
		if (index == iov_count) {
			break;
		}

		memset(&message, 0, sizeof(message));
		message.msg_iov = &pending[index];
		message.msg_iovlen = (size_t) (iov_count - index);
		written = sendmsg(OCLKET_G(socket_fd), &message, MSG_NOSIGNAL);
		if (written < 0) {
			if (errno == EINTR) {
				continue;
			}
			oclket_throw_socket_error("write");
			return 0;
		}
		if (written == 0) {
			errno = EPIPE;
			oclket_throw_socket_error("write");
			return 0;
		}

		remaining = (size_t) written;
		while (index < iov_count && remaining > 0) {
			if (remaining >= pending[index].iov_len) {
				remaining -= pending[index].iov_len;
				index++;
			} else {
				pending[index].iov_base =
					(char *) pending[index].iov_base + remaining;
				pending[index].iov_len -= remaining;
				remaining = 0;
			}
		}
	}

	return 1;
}

static zend_bool oclket_read_all(void *buffer, size_t length)
{
	uint8_t *out = (uint8_t *) buffer;
	size_t offset = 0;

	while (offset < length) {
		ssize_t received = recv(
			OCLKET_G(socket_fd),
			out + offset,
			length - offset,
			0
		);
		if (received < 0) {
			if (errno == EINTR) {
				continue;
			}
			oclket_throw_socket_error("read");
			return 0;
		}
		if (received == 0) {
			errno = ECONNRESET;
			oclket_throw_socket_error("read");
			return 0;
		}
		offset += (size_t) received;
	}

	return 1;
}

static zend_bool oclket_read_packet_header(uint32_t *command, uint32_t *payload_len)
{
	uint8_t header[8];

	if (!oclket_read_all(header, sizeof(header))) {
		return 0;
	}
	*command = oclket_get_u32(header);
	*payload_len = oclket_get_u32(header + 4);
	if (*payload_len > OCLKET_MAX_PAYLOAD) {
		oclket_close_connection();
		zend_throw_exception(zend_ce_exception, "oclket response payload exceeds limit", 0);
		return 0;
	}
	return 1;
}

static zend_bool oclket_discard_payload(uint32_t payload_len)
{
	uint8_t scratch[4096];

	while (payload_len > 0) {
		size_t amount = payload_len < sizeof(scratch) ?
			(size_t) payload_len :
			sizeof(scratch);
		if (!oclket_read_all(scratch, amount)) {
			return 0;
		}
		payload_len -= (uint32_t) amount;
	}
	return 1;
}

static zend_bool oclket_throw_service_error(uint32_t payload_len)
{
	char message[OCLKET_ERROR_BUFFER_SIZE];
	size_t amount = payload_len < sizeof(message) - 1 ?
		(size_t) payload_len :
		sizeof(message) - 1;

	if (amount > 0 && !oclket_read_all(message, amount)) {
		return 0;
	}
	message[amount] = '\0';
	if (!oclket_discard_payload(payload_len - (uint32_t) amount)) {
		return 0;
	}

	if (amount == 0) {
		zend_throw_exception(zend_ce_exception, "oclket service returned an empty error", 0);
	} else {
		zend_throw_exception(zend_ce_exception, message, 0);
	}
	return 0;
}

static zend_bool oclket_expect_ok(void)
{
	uint32_t command;
	uint32_t payload_len;

	if (!oclket_read_packet_header(&command, &payload_len)) {
		return 0;
	}
	if (command == OCLKET_A_ERR) {
		return oclket_throw_service_error(payload_len);
	}
	if (command != OCLKET_A_OK || payload_len != 0) {
		if (!oclket_discard_payload(payload_len)) {
			return 0;
		}
		oclket_close_connection();
		zend_throw_exception(zend_ce_exception, "oclket received an unexpected response", 0);
		return 0;
	}
	return 1;
}

static void oclket_make_frame_header(uint8_t header[8], uint32_t command, uint32_t payload_len)
{
	oclket_put_u32(header, command);
	oclket_put_u32(header + 4, payload_len);
}

static zend_bool oclket_send_key_command(uint32_t command, zend_string *key)
{
	uint8_t header[8];
	struct iovec iov[2];
	size_t key_len = ZSTR_LEN(key);

	if (key_len > OCLKET_MAX_PAYLOAD) {
		zend_value_error("oclket key exceeds the packet size limit");
		return 0;
	}
	if (!oclket_ensure_connection()) {
		return 0;
	}

	oclket_make_frame_header(header, command, (uint32_t) key_len);
	iov[0].iov_base = header;
	iov[0].iov_len = sizeof(header);
	iov[1].iov_base = ZSTR_VAL(key);
	iov[1].iov_len = key_len;
	return oclket_write_iov(iov, 2);
}

static zend_bool oclket_read_ok_response(void)
{
	return oclket_expect_ok();
}

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_MASK_EX(arginfo_oclket_get, 0, 1, MAY_BE_STRING | MAY_BE_FALSE)
	ZEND_ARG_TYPE_INFO(0, key, IS_STRING, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_oclket_set, 0, 2, _IS_BOOL, 0)
	ZEND_ARG_TYPE_INFO(0, key, IS_STRING, 0)
	ZEND_ARG_TYPE_INFO(0, value, IS_STRING, 0)
	ZEND_ARG_TYPE_INFO_WITH_DEFAULT_VALUE(0, ttl, IS_LONG, 0, "0")
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_oclket_del, 0, 1, _IS_BOOL, 0)
	ZEND_ARG_TYPE_INFO(0, key, IS_STRING, 0)
ZEND_END_ARG_INFO()

PHP_FUNCTION(oclket_get)
{
	zend_string *key;
	uint32_t command;
	uint32_t payload_len;

	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_STR(key)
	ZEND_PARSE_PARAMETERS_END();

	if (!oclket_send_key_command(OCLKET_L_GET, key)) {
		RETURN_THROWS();
	}
	if (!oclket_read_packet_header(&command, &payload_len)) {
		RETURN_THROWS();
	}

	if (command == OCLKET_A_ERR) {
		oclket_throw_service_error(payload_len);
		RETURN_THROWS();
	}
	if (command == OCLKET_A_EMP) {
		if (payload_len != 0) {
			if (!oclket_discard_payload(payload_len)) {
				RETURN_THROWS();
			}
			oclket_close_connection();
			zend_throw_exception(zend_ce_exception, "oclket received an invalid empty response", 0);
			RETURN_THROWS();
		}
		RETURN_FALSE;
	}
	if (command != OCLKET_A_ROW) {
		if (!oclket_discard_payload(payload_len)) {
			RETURN_THROWS();
		}
		oclket_close_connection();
		zend_throw_exception(zend_ce_exception, "oclket received an unexpected response", 0);
		RETURN_THROWS();
	}

	{
		zend_string *value = zend_string_alloc(payload_len, 0);
		if (payload_len > 0 && !oclket_read_all(ZSTR_VAL(value), payload_len)) {
			zend_string_release(value);
			RETURN_THROWS();
		}
		ZSTR_VAL(value)[payload_len] = '\0';
		RETURN_STR(value);
	}
}

PHP_FUNCTION(oclket_set)
{
	zend_string *key;
	zend_string *value;
	zend_long ttl = 0;
	size_t key_len;
	size_t value_len;
	size_t payload_len;
	uint8_t header[8];
	uint8_t ttl_bytes[8];
	uint8_t key_len_bytes[4];
	uint8_t value_len_bytes[4];
	struct iovec iov[6];

	ZEND_PARSE_PARAMETERS_START(2, 3)
		Z_PARAM_STR(key)
		Z_PARAM_STR(value)
		Z_PARAM_OPTIONAL
		Z_PARAM_LONG(ttl)
	ZEND_PARSE_PARAMETERS_END();

	if (ttl < 0) {
		zend_value_error("oclket TTL must not be negative");
		RETURN_THROWS();
	}

	key_len = ZSTR_LEN(key);
	value_len = ZSTR_LEN(value);
	if (key_len == 0) {
		zend_value_error("oclket SET key must not be empty");
		RETURN_THROWS();
	}
	if (key_len > UINT32_MAX || value_len > UINT32_MAX ||
		key_len > OCLKET_MAX_PAYLOAD - 16U ||
		value_len > OCLKET_MAX_PAYLOAD - 16U - key_len) {
		zend_value_error("oclket SET payload exceeds the packet size limit");
		RETURN_THROWS();
	}

	payload_len = 16U + key_len + value_len;
	if (!oclket_ensure_connection()) {
		RETURN_THROWS();
	}

	oclket_make_frame_header(header, OCLKET_L_SET, (uint32_t) payload_len);
	oclket_put_u64(ttl_bytes, (uint64_t) ttl);
	oclket_put_u32(key_len_bytes, (uint32_t) key_len);
	oclket_put_u32(value_len_bytes, (uint32_t) value_len);

	iov[0].iov_base = header;
	iov[0].iov_len = sizeof(header);
	iov[1].iov_base = ttl_bytes;
	iov[1].iov_len = sizeof(ttl_bytes);
	iov[2].iov_base = key_len_bytes;
	iov[2].iov_len = sizeof(key_len_bytes);
	iov[3].iov_base = ZSTR_VAL(key);
	iov[3].iov_len = key_len;
	iov[4].iov_base = value_len_bytes;
	iov[4].iov_len = sizeof(value_len_bytes);
	iov[5].iov_base = ZSTR_VAL(value);
	iov[5].iov_len = value_len;

	if (!oclket_write_iov(iov, 6) || !oclket_read_ok_response()) {
		RETURN_THROWS();
	}
	RETURN_TRUE;
}

PHP_FUNCTION(oclket_del)
{
	zend_string *key;

	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_STR(key)
	ZEND_PARSE_PARAMETERS_END();

	if (!oclket_send_key_command(OCLKET_L_DEL, key) || !oclket_read_ok_response()) {
		RETURN_THROWS();
	}
	RETURN_TRUE;
}

static const zend_function_entry oclket_functions[] = {
	PHP_FE(oclket_get, arginfo_oclket_get)
	PHP_FE(oclket_set, arginfo_oclket_set)
	PHP_FE(oclket_del, arginfo_oclket_del)
	PHP_FE_END
};

PHP_MINIT_FUNCTION(oclket)
{
	REGISTER_INI_ENTRIES();
	return SUCCESS;
}

PHP_MSHUTDOWN_FUNCTION(oclket)
{
	UNREGISTER_INI_ENTRIES();
	return SUCCESS;
}

PHP_MINFO_FUNCTION(oclket)
{
	php_info_print_table_start();
	php_info_print_table_row(2, "Oclket support", "enabled");
	php_info_print_table_row(2, "Version", PHP_OCLKET_VERSION);
	php_info_print_table_row(2, "Socket path", OCLKET_G(socket_path));
	php_info_print_table_end();
	DISPLAY_INI_ENTRIES();
}

zend_module_entry oclket_module_entry = {
	STANDARD_MODULE_HEADER,
	"oclket",
	oclket_functions,
	PHP_MINIT(oclket),
	PHP_MSHUTDOWN(oclket),
	NULL,
	NULL,
	PHP_MINFO(oclket),
	PHP_OCLKET_VERSION,
	PHP_MODULE_GLOBALS(oclket),
	PHP_GINIT(oclket),
	PHP_GSHUTDOWN(oclket),
	NULL,
	STANDARD_MODULE_PROPERTIES_EX
};

#if defined(COMPILE_DL_OCLKET) || defined(COMPILE_DL_EXT) || defined(ZEND_COMPILE_DL_EXT)
#ifdef ZTS
ZEND_TSRMLS_CACHE_DEFINE()
#endif
ZEND_GET_MODULE(oclket)
#endif
