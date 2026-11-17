#include "php.h"
#include "ext/standard/info.h"
#include "ext/standard/php_var.h"
#include "Zend/zend_exceptions.h"
#include "Zend/zend_smart_str.h"

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

static const char oclket_binary_prefix[] = "\0OCKB\x01";
static const char oclket_legacy_prefix[] = "\0OCLKET-PHP-1";

enum {
	OCLKET_VALUE_NULL = 0,
	OCLKET_VALUE_FALSE = 1,
	OCLKET_VALUE_TRUE = 2,
	OCLKET_VALUE_INTEGER = 3,
	OCLKET_VALUE_DOUBLE = 4,
	OCLKET_VALUE_STRING = 5,
	OCLKET_VALUE_ARRAY = 6,
	OCLKET_VALUE_OBJECT = 7,
	OCLKET_ARRAY_KEY_INTEGER = 16,
	OCLKET_ARRAY_KEY_STRING = 17,
	OCLKET_CODEC_MAX_DEPTH = 128
};

typedef struct {
	const unsigned char *cursor;
	const unsigned char *end;
} oclket_codec_reader;

static zend_bool oclket_codec_error(const char *message)
{
	if (!EG(exception)) {
		zend_throw_exception(zend_ce_exception, message, 0);
	}
	return 0;
}

static zend_bool oclket_codec_is_stdclass(zend_class_entry *ce)
{
	return ZSTR_LEN(ce->name) == sizeof("stdClass") - 1 &&
		memcmp(ZSTR_VAL(ce->name), "stdClass", sizeof("stdClass") - 1) == 0;
}

static zend_bool oclket_codec_append_bytes(smart_str *buffer, const char *bytes, size_t length, size_t limit)
{
	size_t current = buffer->s == NULL ? 0 : ZSTR_LEN(buffer->s);
	if (current > limit || length > limit - current) {
		return oclket_codec_error("oclket SET payload exceeds the packet size limit");
	}
	smart_str_appendl(buffer, bytes, length);
	return 1;
}

static zend_bool oclket_codec_append_byte(smart_str *buffer, uint8_t byte, size_t limit)
{
	char value = (char) byte;
	return oclket_codec_append_bytes(buffer, &value, 1, limit);
}

static zend_bool oclket_codec_append_uvarint(smart_str *buffer, uint64_t value, size_t limit)
{
	unsigned char bytes[10];
	size_t length = 0;
	do {
		bytes[length] = (unsigned char) (value & 0x7f);
		value >>= 7;
		if (value != 0) {
			bytes[length] |= 0x80;
		}
		length++;
	} while (value != 0);
	return oclket_codec_append_bytes(buffer, (const char *) bytes, length, limit);
}

static zend_bool oclket_codec_read_byte(oclket_codec_reader *reader, uint8_t *value)
{
	if (reader->cursor >= reader->end) {
		return oclket_codec_error("oclket received truncated binary data");
	}
	*value = *reader->cursor++;
	return 1;
}

static zend_bool oclket_codec_read_uvarint(oclket_codec_reader *reader, uint64_t *value)
{
	uint64_t result;
	uint8_t byte;

	if (reader->cursor >= reader->end) {
		return oclket_codec_error("oclket received truncated binary data");
	}
	byte = *reader->cursor++;
	if ((byte & 0x80) == 0) {
		*value = byte;
		return 1;
	}
	result = byte & 0x7f;

	if (reader->cursor >= reader->end) {
		return oclket_codec_error("oclket received truncated binary data");
	}
	byte = *reader->cursor++;
	result |= (uint64_t) (byte & 0x7f) << 7;
	if ((byte & 0x80) == 0) {
		*value = result;
		return 1;
	}

	for (unsigned int index = 2, shift = 14; index < 10; index++, shift += 7) {
		if (!oclket_codec_read_byte(reader, &byte)) {
			return 0;
		}
		if (index == 9 && byte > 1) {
			return oclket_codec_error("oclket received an invalid binary integer");
		}
		result |= ((uint64_t) (byte & 0x7f)) << shift;
		if ((byte & 0x80) == 0) {
			*value = result;
			return 1;
		}
	}
	return oclket_codec_error("oclket received an invalid binary integer");
}

static zend_bool oclket_codec_read_string(oclket_codec_reader *reader, zend_string **value)
{
	uint64_t length;

	if (!oclket_codec_read_uvarint(reader, &length)) {
		return 0;
	}
	if (length > (uint64_t) (reader->end - reader->cursor) || length > SIZE_MAX) {
		return oclket_codec_error("oclket received an invalid binary string length");
	}
	*value = zend_string_init((const char *) reader->cursor, (size_t) length, 0);
	reader->cursor += (size_t) length;
	return 1;
}

static zend_bool oclket_codec_read_key_string(oclket_codec_reader *reader, zend_string **value)
{
	uint64_t length;
	zend_string *key;

	if (!oclket_codec_read_uvarint(reader, &length)) {
		return 0;
	}
	if (length > (uint64_t) (reader->end - reader->cursor) || length > SIZE_MAX) {
		return oclket_codec_error("oclket received an invalid binary key length");
	}

	key = zend_string_init((const char *) reader->cursor, (size_t) length, 0);
	reader->cursor += (size_t) length;
	*value = key;
	return 1;
}

static zend_bool oclket_codec_encode_value(smart_str *buffer, zval *value, unsigned int depth, size_t limit)
{
	HashTable *properties;
	zend_ulong numeric_key;
	zend_string *string_key;
	zval *item;

	if (depth > OCLKET_CODEC_MAX_DEPTH) {
		return oclket_codec_error("oclket value nesting exceeds the binary codec limit");
	}
	ZVAL_DEREF(value);
	if (Z_TYPE_P(value) == IS_INDIRECT) {
		value = Z_INDIRECT_P(value);
		ZVAL_DEREF(value);
	}

	switch (Z_TYPE_P(value)) {
		case IS_NULL:
			return oclket_codec_append_byte(buffer, OCLKET_VALUE_NULL, limit);
		case IS_FALSE:
			return oclket_codec_append_byte(buffer, OCLKET_VALUE_FALSE, limit);
		case IS_TRUE:
			return oclket_codec_append_byte(buffer, OCLKET_VALUE_TRUE, limit);
		case IS_LONG: {
			uint64_t number = (uint64_t) Z_LVAL_P(value);
			uint64_t zigzag = (number << 1) ^ (uint64_t) -(Z_LVAL_P(value) < 0);
			return oclket_codec_append_byte(buffer, OCLKET_VALUE_INTEGER, limit) &&
				oclket_codec_append_uvarint(buffer, zigzag, limit);
		}
		case IS_DOUBLE: {
			uint64_t bits;
			unsigned char bytes[8];
			memcpy(&bits, &Z_DVAL_P(value), sizeof(bits));
			for (unsigned int index = 0; index < sizeof(bytes); index++) {
				bytes[index] = (unsigned char) (bits >> (56 - index * 8));
			}
			return oclket_codec_append_byte(buffer, OCLKET_VALUE_DOUBLE, limit) &&
				oclket_codec_append_bytes(buffer, (const char *) bytes, sizeof(bytes), limit);
		}
		case IS_STRING:
			return oclket_codec_append_byte(buffer, OCLKET_VALUE_STRING, limit) &&
				oclket_codec_append_uvarint(buffer, Z_STRLEN_P(value), limit) &&
				oclket_codec_append_bytes(buffer, Z_STRVAL_P(value), Z_STRLEN_P(value), limit);
		case IS_ARRAY: {
			HashTable *array = Z_ARRVAL_P(value);
			if (!oclket_codec_append_byte(buffer, OCLKET_VALUE_ARRAY, limit) ||
				!oclket_codec_append_uvarint(buffer, zend_hash_num_elements(array), limit)) {
				return 0;
			}
			ZEND_HASH_FOREACH_KEY_VAL(array, numeric_key, string_key, item) {
				if (string_key == NULL) {
					uint64_t number = (uint64_t) numeric_key;
					uint64_t zigzag = (number << 1) ^ (uint64_t) -(numeric_key < 0);
					if (!oclket_codec_append_byte(buffer, OCLKET_ARRAY_KEY_INTEGER, limit) ||
						!oclket_codec_append_uvarint(buffer, zigzag, limit)) {
						return 0;
					}
				} else {
					if (!oclket_codec_append_byte(buffer, OCLKET_ARRAY_KEY_STRING, limit) ||
						!oclket_codec_append_uvarint(buffer, ZSTR_LEN(string_key), limit) ||
						!oclket_codec_append_bytes(buffer, ZSTR_VAL(string_key), ZSTR_LEN(string_key), limit)) {
						return 0;
					}
				}
				if (!oclket_codec_encode_value(buffer, item, depth + 1, limit)) {
					return 0;
				}
			} ZEND_HASH_FOREACH_END();
			return 1;
		}
		case IS_OBJECT: {
			zend_class_entry *ce = Z_OBJCE_P(value);
			size_t property_count = 0;
			if (ce->ce_flags & ZEND_ACC_ENUM ||
				(!oclket_codec_is_stdclass(ce) && ce->type == ZEND_INTERNAL_CLASS) ||
				zend_hash_str_exists(&ce->function_table, "__serialize", sizeof("__serialize") - 1) ||
				zend_hash_str_exists(&ce->function_table, "__sleep", sizeof("__sleep") - 1)) {
				return oclket_codec_error("oclket binary codec supports only ordinary user objects");
			}
			properties = Z_OBJPROP_P(value);
			ZEND_HASH_FOREACH_KEY_VAL(properties, numeric_key, string_key, item) {
				if (string_key == NULL ||
					memchr(ZSTR_VAL(string_key), '\0', ZSTR_LEN(string_key)) != NULL) {
					return oclket_codec_error("oclket binary codec does not support non-public object properties");
				}
				if (Z_TYPE_P(item) != IS_UNDEF) {
					property_count++;
				}
			} ZEND_HASH_FOREACH_END();

			if (!oclket_codec_append_byte(buffer, OCLKET_VALUE_OBJECT, limit) ||
				!oclket_codec_append_uvarint(buffer, ZSTR_LEN(ce->name), limit) ||
				!oclket_codec_append_bytes(buffer, ZSTR_VAL(ce->name), ZSTR_LEN(ce->name), limit) ||
				!oclket_codec_append_uvarint(buffer, property_count, limit)) {
				return 0;
			}
			ZEND_HASH_FOREACH_STR_KEY_VAL(properties, string_key, item) {
				if (Z_TYPE_P(item) == IS_UNDEF) {
					continue;
				}
				if (!oclket_codec_append_byte(buffer, OCLKET_ARRAY_KEY_STRING, limit) ||
					!oclket_codec_append_uvarint(buffer, ZSTR_LEN(string_key), limit) ||
					!oclket_codec_append_bytes(buffer, ZSTR_VAL(string_key), ZSTR_LEN(string_key), limit) ||
					!oclket_codec_encode_value(buffer, item, depth + 1, limit)) {
					return 0;
				}
			} ZEND_HASH_FOREACH_END();
			return 1;
		}
		default:
			return oclket_codec_error("oclket binary codec does not support this PHP value type");
	}
}

static zend_bool oclket_codec_decode_value(oclket_codec_reader *reader, zval *value, unsigned int depth)
{
	uint8_t type;
	uint64_t count;

	if (depth > OCLKET_CODEC_MAX_DEPTH) {
		return oclket_codec_error("oclket value nesting exceeds the binary codec limit");
	}
	if (!oclket_codec_read_byte(reader, &type)) {
		return 0;
	}

	switch (type) {
		case OCLKET_VALUE_NULL:
			ZVAL_NULL(value);
			return 1;
		case OCLKET_VALUE_FALSE:
			ZVAL_FALSE(value);
			return 1;
		case OCLKET_VALUE_TRUE:
			ZVAL_TRUE(value);
			return 1;
		case OCLKET_VALUE_INTEGER: {
			uint64_t zigzag;
			int64_t number;
			if (!oclket_codec_read_uvarint(reader, &zigzag)) {
				return 0;
			}
			number = (int64_t) (zigzag >> 1) ^ -(int64_t) (zigzag & 1);
			if (number < ZEND_LONG_MIN || number > ZEND_LONG_MAX) {
				return oclket_codec_error("oclket binary integer is outside the PHP integer range");
			}
			ZVAL_LONG(value, (zend_long) number);
			return 1;
		}
		case OCLKET_VALUE_DOUBLE: {
			uint64_t bits = 0;
			double number;
			if ((size_t) (reader->end - reader->cursor) < sizeof(bits)) {
				return oclket_codec_error("oclket received truncated binary data");
			}
			for (unsigned int index = 0; index < sizeof(bits); index++) {
				bits = (bits << 8) | reader->cursor[index];
			}
			reader->cursor += sizeof(bits);
			memcpy(&number, &bits, sizeof(number));
			ZVAL_DOUBLE(value, number);
			return 1;
		}
		case OCLKET_VALUE_STRING: {
			zend_string *string;
			if (!oclket_codec_read_string(reader, &string)) {
				return 0;
			}
			ZVAL_STR(value, string);
			return 1;
		}
		case OCLKET_VALUE_ARRAY:
			if (!oclket_codec_read_uvarint(reader, &count) || count > (uint64_t) (reader->end - reader->cursor) / 2) {
				return oclket_codec_error("oclket received an invalid binary array length");
			}
			array_init_size(value, (uint32_t) (count > UINT32_MAX ? UINT32_MAX : count));
			for (uint64_t index = 0; index < count; index++) {
				uint8_t key_type;
				zval item;
				ZVAL_UNDEF(&item);
				if (!oclket_codec_read_byte(reader, &key_type)) {
					return 0;
				}
				if (key_type == OCLKET_ARRAY_KEY_INTEGER) {
					uint64_t zigzag;
					int64_t key;
					if (!oclket_codec_read_uvarint(reader, &zigzag)) {
						return 0;
					}
					key = (int64_t) (zigzag >> 1) ^ -(int64_t) (zigzag & 1);
					if (key < ZEND_LONG_MIN || key > ZEND_LONG_MAX) {
						return oclket_codec_error("oclket binary array key is outside the PHP integer range");
					}
					if (!oclket_codec_decode_value(reader, &item, depth + 1)) {
						if (!Z_ISUNDEF(item)) {
							zval_ptr_dtor(&item);
						}
						return 0;
					}
					zend_hash_index_update(Z_ARRVAL_P(value), (zend_ulong) (zend_long) key, &item);
				} else if (key_type == OCLKET_ARRAY_KEY_STRING) {
					zend_string *key;
					if (!oclket_codec_read_key_string(reader, &key)) {
						return 0;
					}
					if (!oclket_codec_decode_value(reader, &item, depth + 1)) {
						zend_string_release(key);
						if (!Z_ISUNDEF(item)) {
							zval_ptr_dtor(&item);
						}
						return 0;
					}
					zend_hash_update(Z_ARRVAL_P(value), key, &item);
					zend_string_release(key);
				} else {
					return oclket_codec_error("oclket received an invalid binary array key");
				}
			}
			return 1;
		case OCLKET_VALUE_OBJECT: {
			zend_string *class_name;
			zend_class_entry *ce;
			if (!oclket_codec_read_string(reader, &class_name)) {
				return oclket_codec_error("oclket received an invalid binary object");
			}
			if (!oclket_codec_read_uvarint(reader, &count) ||
				count > (uint64_t) (reader->end - reader->cursor) / 2) {
				zend_string_release(class_name);
				return oclket_codec_error("oclket received an invalid binary object");
			}
			ce = zend_lookup_class_ex(class_name, NULL, ZEND_FETCH_CLASS_NO_AUTOLOAD);
			if (ce == NULL || (ce->ce_flags & (ZEND_ACC_ABSTRACT | ZEND_ACC_INTERFACE | ZEND_ACC_TRAIT | ZEND_ACC_ENUM)) ||
				(!oclket_codec_is_stdclass(ce) && ce->type == ZEND_INTERNAL_CLASS)) {
				zend_string_release(class_name);
				return oclket_codec_error("oclket binary object class is unavailable or unsupported");
			}
			object_init_ex(value, ce);
			zend_string_release(class_name);
			if (EG(exception)) {
				return 0;
			}
			for (uint64_t index = 0; index < count; index++) {
				uint8_t key_type;
				zend_string *key;
				zval item;
				zend_property_info *property_info;
				ZVAL_UNDEF(&item);
				if (!oclket_codec_read_byte(reader, &key_type) ||
					key_type != OCLKET_ARRAY_KEY_STRING ||
					!oclket_codec_read_key_string(reader, &key)) {
					return oclket_codec_error("oclket received an invalid binary object property");
				}
				if (memchr(ZSTR_VAL(key), '\0', ZSTR_LEN(key)) != NULL ||
					!oclket_codec_decode_value(reader, &item, depth + 1)) {
					zend_string_release(key);
					if (!Z_ISUNDEF(item)) {
						zval_ptr_dtor(&item);
					}
					return 0;
				}
				property_info = zend_hash_find_ptr(&ce->properties_info, key);
				if (property_info != NULL && !(property_info->flags & ZEND_ACC_PUBLIC)) {
					zend_string_release(key);
					zval_ptr_dtor(&item);
					return oclket_codec_error("oclket binary object contains a non-public property");
				}
				if (property_info != NULL) {
					zend_update_property_ex(ce, Z_OBJ_P(value), key, &item);
				} else {
					Z_TRY_ADDREF(item);
					zend_hash_update(Z_OBJPROP_P(value), key, &item);
				}
				zval_ptr_dtor(&item);
				zend_string_release(key);
				if (EG(exception)) {
					return 0;
				}
			}
			return 1;
		}
		default:
			return oclket_codec_error("oclket received an unknown binary value type");
	}
}

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

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_oclket_get, 0, 1, IS_MIXED, 0)
	ZEND_ARG_TYPE_INFO(0, key, IS_STRING, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_oclket_set, 0, 2, _IS_BOOL, 0)
	ZEND_ARG_TYPE_INFO(0, key, IS_STRING, 0)
	ZEND_ARG_TYPE_INFO(0, value, IS_MIXED, 0)
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
		zend_string *serialized = zend_string_alloc(payload_len, 0);
		const unsigned char *cursor;
		const unsigned char *end;
		php_unserialize_data_t var_hash;
		zval value;
		int result;

		if (payload_len > 0 && !oclket_read_all(ZSTR_VAL(serialized), payload_len)) {
			zend_string_release(serialized);
			RETURN_THROWS();
		}
		ZSTR_VAL(serialized)[payload_len] = '\0';
		if (payload_len >= sizeof(oclket_binary_prefix) - 1 &&
			memcmp(ZSTR_VAL(serialized), oclket_binary_prefix,
				sizeof(oclket_binary_prefix) - 1) == 0) {
			const unsigned char *value_start =
				(const unsigned char *) ZSTR_VAL(serialized) + sizeof(oclket_binary_prefix) - 1;
			const unsigned char *value_end =
				(const unsigned char *) ZSTR_VAL(serialized) + payload_len;
			oclket_codec_reader reader;
			zval value;

			reader.cursor = value_start;
			reader.end = value_end;
			ZVAL_UNDEF(&value);
			if (!oclket_codec_decode_value(&reader, &value, 0) || reader.cursor != reader.end) {
				if (!Z_ISUNDEF(value)) {
					zval_ptr_dtor(&value);
				}
				zend_string_release(serialized);
				if (!EG(exception)) {
					oclket_codec_error("oclket received invalid or trailing binary data");
				}
				RETURN_THROWS();
			}
			zend_string_release(serialized);
			ZVAL_COPY_VALUE(return_value, &value);
			return;
		}

		if (payload_len < sizeof(oclket_legacy_prefix) - 1 ||
			memcmp(ZSTR_VAL(serialized), oclket_legacy_prefix,
				sizeof(oclket_legacy_prefix) - 1) != 0) {
			RETURN_STR(serialized);
		}

		cursor = (const unsigned char *) ZSTR_VAL(serialized) + sizeof(oclket_legacy_prefix) - 1;
		end = (const unsigned char *) ZSTR_VAL(serialized) + payload_len;
		ZVAL_UNDEF(&value);
		PHP_VAR_UNSERIALIZE_INIT(var_hash);
		result = php_var_unserialize(&value, &cursor, end, &var_hash);
		PHP_VAR_UNSERIALIZE_DESTROY(var_hash);
		zend_string_release(serialized);

		if (EG(exception)) {
			if (!Z_ISUNDEF(value)) {
				zval_ptr_dtor(&value);
			}
			RETURN_THROWS();
		}
		if (!result || cursor != end) {
			if (!Z_ISUNDEF(value)) {
				zval_ptr_dtor(&value);
			}
			zend_throw_exception(zend_ce_exception, "oclket received an invalid serialized value", 0);
			RETURN_THROWS();
		}

		ZVAL_COPY_VALUE(return_value, &value);
	}
}

PHP_FUNCTION(oclket_set)
{
	zend_string *key;
	zval *value;
	smart_str serialized = {0};
	zend_long ttl = 0;
	size_t key_len;
	size_t value_len;
	size_t payload_len;
	size_t encoded_limit;
	uint8_t header[8];
	uint8_t ttl_bytes[8];
	uint8_t key_len_bytes[4];
	uint8_t value_len_bytes[4];
	struct iovec iov[6];

	ZEND_PARSE_PARAMETERS_START(2, 3)
		Z_PARAM_STR(key)
		Z_PARAM_ZVAL(value)
		Z_PARAM_OPTIONAL
		Z_PARAM_LONG(ttl)
	ZEND_PARSE_PARAMETERS_END();

	if (ttl < 0) {
		zend_value_error("oclket TTL must not be negative");
		RETURN_THROWS();
	}

	key_len = ZSTR_LEN(key);
	if (key_len == 0) {
		zend_value_error("oclket SET key must not be empty");
		RETURN_THROWS();
	}
	if (key_len > UINT32_MAX || key_len > OCLKET_MAX_PAYLOAD - 16U) {
		zend_value_error("oclket SET payload exceeds the packet size limit");
		RETURN_THROWS();
	}

	encoded_limit = OCLKET_MAX_PAYLOAD - 16U - key_len;
	if (!oclket_codec_append_bytes(&serialized, oclket_binary_prefix,
			sizeof(oclket_binary_prefix) - 1, encoded_limit)) {
		smart_str_free(&serialized);
		RETURN_THROWS();
	}
	if (!oclket_codec_encode_value(&serialized, value, 0, encoded_limit) || EG(exception)) {
		smart_str_free(&serialized);
		RETURN_THROWS();
	}
	smart_str_0(&serialized);
	if (serialized.s == NULL) {
		zend_throw_exception(zend_ce_exception, "oclket could not encode the value", 0);
		RETURN_THROWS();
	}

	value_len = ZSTR_LEN(serialized.s);
	if (value_len > UINT32_MAX) {
		smart_str_free(&serialized);
		zend_value_error("oclket SET payload exceeds the packet size limit");
		RETURN_THROWS();
	}

	payload_len = 16U + key_len + value_len;
	if (!oclket_ensure_connection()) {
		smart_str_free(&serialized);
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
	iov[5].iov_base = ZSTR_VAL(serialized.s);
	iov[5].iov_len = value_len;

	if (!oclket_write_iov(iov, 6) || !oclket_read_ok_response()) {
		smart_str_free(&serialized);
		RETURN_THROWS();
	}
	smart_str_free(&serialized);
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
