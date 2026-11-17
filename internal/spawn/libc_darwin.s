//go:build darwin

#include "textflag.h"

// One trampoline per libSystem function, the way golang.org/x/sys/unix reaches libc without cgo.

TEXT libc_posix_spawn_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn(SB)
GLOBL	·posixSpawnAddr(SB), RODATA, $8
DATA	·posixSpawnAddr(SB)/8, $libc_posix_spawn_trampoline<>(SB)

TEXT libc_posix_spawnattr_init_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_init(SB)
GLOBL	·attrInitAddr(SB), RODATA, $8
DATA	·attrInitAddr(SB)/8, $libc_posix_spawnattr_init_trampoline<>(SB)

TEXT libc_posix_spawnattr_destroy_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_destroy(SB)
GLOBL	·attrDestroyAddr(SB), RODATA, $8
DATA	·attrDestroyAddr(SB)/8, $libc_posix_spawnattr_destroy_trampoline<>(SB)

TEXT libc_posix_spawnattr_setflags_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_setflags(SB)
GLOBL	·attrSetFlagsAddr(SB), RODATA, $8
DATA	·attrSetFlagsAddr(SB)/8, $libc_posix_spawnattr_setflags_trampoline<>(SB)

TEXT libc_posix_spawnattr_setsigmask_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_setsigmask(SB)
GLOBL	·attrSetSigmaskAddr(SB), RODATA, $8
DATA	·attrSetSigmaskAddr(SB)/8, $libc_posix_spawnattr_setsigmask_trampoline<>(SB)

TEXT libc_posix_spawn_file_actions_init_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn_file_actions_init(SB)
GLOBL	·actionsInitAddr(SB), RODATA, $8
DATA	·actionsInitAddr(SB)/8, $libc_posix_spawn_file_actions_init_trampoline<>(SB)

TEXT libc_posix_spawn_file_actions_destroy_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn_file_actions_destroy(SB)
GLOBL	·actionsDestroyAddr(SB), RODATA, $8
DATA	·actionsDestroyAddr(SB)/8, $libc_posix_spawn_file_actions_destroy_trampoline<>(SB)

TEXT libc_posix_spawn_file_actions_adddup2_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn_file_actions_adddup2(SB)
GLOBL	·actionsDup2Addr(SB), RODATA, $8
DATA	·actionsDup2Addr(SB)/8, $libc_posix_spawn_file_actions_adddup2_trampoline<>(SB)

TEXT libc_posix_spawn_file_actions_addinherit_np_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn_file_actions_addinherit_np(SB)
GLOBL	·actionsInheritAddr(SB), RODATA, $8
DATA	·actionsInheritAddr(SB)/8, $libc_posix_spawn_file_actions_addinherit_np_trampoline<>(SB)
