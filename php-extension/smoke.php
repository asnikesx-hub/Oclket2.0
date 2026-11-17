<?php

$key = 'oclket-php-smoke-' . bin2hex(random_bytes(12));

try {
    if (oclket_get($key) !== false) {
        throw new RuntimeException('Expected an initially missing key');
    }

    $values = [
        "binary" => "\0binary\0value\xff",
        "empty string" => "",
        "integer" => 42,
        "float" => 3.125,
        "true" => true,
        "false" => false,
        "null" => null,
        "array" => ["nested" => [1, "two", false]],
        "repeated keys" => array_fill(0, 500, ["id" => 1, "name" => "test", "ok" => true]),
    ];
    foreach ($values as $label => $expected) {
        if (oclket_set($key, $expected) !== true || oclket_get($key) !== $expected) {
            throw new RuntimeException("GET did not preserve the $label value");
        }
    }

    $object = (object) ["answer" => 42];
    if (oclket_set($key, $object) !== true) {
        throw new RuntimeException('SET did not accept an object');
    }
    $actual = oclket_get($key);
    if (!($actual instanceof stdClass) || $actual->answer !== $object->answer) {
        throw new RuntimeException('GET did not preserve the object value');
    }

    if (oclket_del($key) !== true || oclket_get($key) !== false) {
        throw new RuntimeException('DEL did not remove the key');
    }
} finally {
    oclket_del($key);
}

echo "Oclket extension smoke test passed\n";
