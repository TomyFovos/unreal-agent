@echo off
rem Drop files on this file to copy them into a Docker Sandbox for Unreal Agent.
rem The sandbox is this file's name: rename a copy to devlore.cmd, asp.cmd ...
rem Files land in /home/agent/drop; their paths go to the clipboard, ready to
rem paste into Unreal.
setlocal EnableDelayedExpansion
set "SB=%~n0"
set "SBX=%LOCALAPPDATA%\DockerSandboxes\bin\sbx.exe"
if not exist "%SBX%" set "SBX=sbx"
if "%~1"=="" (
  echo Drop files on %~nx0 to send them to the sandbox %SB%.
  pause
  exit /b 1
)
"%SBX%" exec %SB% mkdir -p /home/agent/drop >nul 2>&1
set "LIST="
for %%F in (%*) do (
  "%SBX%" cp "%%~fF" "%SB%:/home/agent/drop/%%~nxF"
  if errorlevel 1 (
    echo FAILED: %%~fF
  ) else (
    echo sent: /home/agent/drop/%%~nxF
    set "LIST=!LIST! "/home/agent/drop/%%~nxF""
  )
)
if defined LIST (
  <nul set /p "=!LIST:~1!" | clip
  echo.
  echo The paths are on the clipboard. Paste them into Unreal.
)
ping -n 5 127.0.0.1 >nul
