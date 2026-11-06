PHP_ARG_ENABLE(
  [oclket],
  [whether to enable oclket support],
  [AS_HELP_STRING([--enable-oclket], [Enable Oclket Unix socket support])],
  [no]
)

if test "$PHP_OCLKET" != "no"; then
  PHP_NEW_EXTENSION([oclket], [oclket.c], [$ext_shared])
fi
