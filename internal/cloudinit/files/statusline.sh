#!/usr/bin/env bash
# Catppuccin Mocha statusline for Claude Code
# Shows: model | folder | branch | context | price

# Read JSON input
input=$(cat)

# Opt-in diagnostic capture: when /tmp/statusline-debug exists, append
# claude's piped JSON to /tmp/statusline-input.jsonl on every render.
# Useful when the bar shows fallback values ("claude" / empty folder /
# 0/0 context) so we can see exactly what claude provides and adjust
# the field paths below.
#   touch /tmp/statusline-debug   # enable capture
#   rm    /tmp/statusline-debug   # disable
if [ -e /tmp/statusline-debug ]; then
    echo "$input" >> /tmp/statusline-input.jsonl
fi

# Separator (simple chevron like nvim)
SEP="›"  # Use › or > or │

# Catppuccin Mocha colors (matching Ghostty theme)
# Base colors
BG="\033[48;5;235m"           # #1e1e2e (background)
FG="\033[38;5;189m"           # #cdd6f4 (foreground)

# Accent colors (using Catppuccin palette)
BLUE="\033[38;5;117m"         # #89b4fa
GREEN="\033[38;5;151m"        # #a6e3a1
YELLOW="\033[38;5;223m"       # #f9e2af
PINK="\033[38;5;218m"         # #f5c2e7
PEACH="\033[38;5;216m"        # #fab387
MAUVE="\033[38;5;183m"        # #cba6f7
TEAL="\033[38;5;152m"         # #94e2d5
GRAY="\033[38;5;240m"         # #45475a

# Dim foreground for separators
DIM="\033[2m\033[38;5;240m"   # Dimmed gray

RESET="\033[0m"
BOLD="\033[1m"

# Extract data from JSON input
# cwd: try the rich workspace object first, then the flat .cwd, then
# fall back to $PWD so the folder segment never silently empties out
# when claude's JSON shape drifts.
cwd=$(echo "$input" | jq -r '.workspace.current_dir // .workspace.project_dir // .cwd // empty')
[ -z "$cwd" ] && cwd="$PWD"

# Model: prefer the structured .model.id; fall back to .model.display_name
# then to a bare-string .model. The case-statement below normalises
# whatever lands in $model down to "opus"/"sonnet"/"haiku" or the
# verbatim string when it's something we don't recognise.
model=$(echo "$input" | jq -r '
    if (.model | type) == "object" then
        (.model.id // .model.display_name)
    else
        .model
    end // empty')

# Extract context window information - use current_usage (resets on /compact) not total (cumulative)
current_input=$(echo "$input" | jq -r '.context_window.current_usage.input_tokens // 0')
cache_creation=$(echo "$input" | jq -r '.context_window.current_usage.cache_creation_input_tokens // 0')
cache_read=$(echo "$input" | jq -r '.context_window.current_usage.cache_read_input_tokens // 0')
context_used=$((current_input + cache_creation + cache_read))
context_limit=$(echo "$input" | jq -r '.context_window.context_window_size // 200000')

# Use Claude Code's pre-calculated percentage (accurate after /compact or /clear)
context_pct=$(echo "$input" | jq -r '.context_window.used_percentage // 0' | cut -d. -f1)

# Use total cost if available, otherwise calculate from current usage
total_cost=$(echo "$input" | jq -r '.cost.total_cost_usd // empty')
if [[ -n "$total_cost" ]]; then
    cost=$(printf "%.4f" "$total_cost")
else
    input_tokens="$current_input"
    output_tokens=$(echo "$input" | jq -r '.context_window.current_usage.output_tokens // 0')
    cost=$(awk -v input="${input_tokens:-0}" -v output="${output_tokens:-0}" 'BEGIN {printf "%.4f", (input * 3 + output * 15) / 1000000}')
fi

# Format context display
if [[ "${context_used:-0}" -gt 1000 ]]; then
    context_display=$(awk -v used="${context_used:-0}" 'BEGIN {printf "%.1fk", used / 1000}')
else
    context_display="${context_used:-0}"
fi

if [[ "${context_limit:-0}" -gt 1000 ]]; then
    limit_display=$(awk -v limit="${context_limit:-0}" 'BEGIN {printf "%.0fk", limit / 1000}')
else
    limit_display="${context_limit:-0}"
fi

# Get folder name
folder=$(basename "$cwd")

# Get git branch
branch=""
if git -C "$cwd" rev-parse --git-dir &>/dev/null 2>&1; then
    branch=$(git -C "$cwd" symbolic-ref --short HEAD 2>/dev/null || git -C "$cwd" rev-parse --short HEAD 2>/dev/null)

    # Get ahead/behind counts
    if counts=$(git -C "$cwd" rev-list --left-right --count @{upstream}...HEAD 2>/dev/null); then
        behind=${counts%%	*}
        ahead=${counts##*	}

        if [[ "$ahead" -gt 0 ]]; then
            branch="$branch ↑$ahead"
        fi
        if [[ "$behind" -gt 0 ]]; then
            branch="$branch ↓$behind"
        fi
    fi
else
    branch="no git"
fi

# Map model names to short versions. Match case-insensitively so the
# id form ("claude-opus-4-7") and the display form ("Opus 4.7") both
# normalise the same way. Default to whatever claude reported (last
# path segment) rather than a hard-coded "claude" string — an
# unanticipated model name still shows something informative.
#
# Use `tr` rather than the bash 4.x `${var,,}` lowercase expansion:
# macOS ships bash 3.2 (frozen since 2007 over GPLv3), so the
# expansion form aborts the script with "bad substitution" if anyone
# runs it locally for diagnostics. `tr` works everywhere.
model_lc=$(printf '%s' "$model" | tr '[:upper:]' '[:lower:]')
case "$model_lc" in
    *"opus"*)   model_short="opus" ;;
    *"sonnet"*) model_short="sonnet" ;;
    *"haiku"*)  model_short="haiku" ;;
    "")         model_short="claude" ;;
    *)          model_short="${model##*/}" ;;
esac

# Build statusline segments (nvim-style with icons and separators)
# Format: color + icon + text + dim separator

# Segment 1: Model (with lightning icon)
segment1="${BLUE}${BOLD}${model_short}${RESET}"

# Segment 2: Folder (with folder icon)
segment2="${TEAL} ${folder}${RESET}"

# Segment 3: Branch (with git branch icon)
segment3="${GREEN} ${branch}${RESET}"

# Segment 4: Context (with dynamic icon based on usage)
context_color="$MAUVE"
context_icon=""
if [[ "$context_pct" -ge 90 ]]; then
    context_color="$PEACH"
    context_icon=" "  # Warning
elif [[ "$context_pct" -ge 75 ]]; then
    context_color="$YELLOW"
    context_icon=" "  # Alert
else
    context_icon=" "  # Database/memory
fi
segment4="${context_color}${context_icon} ${context_display}/${limit_display} ${context_pct}%${RESET}"

# Segment 5: Price (with dollar icon)
segment5="${PEACH} \$${cost}${RESET}"

# Combine all segments with dim separators
printf '%b' "${segment1} ${DIM}${SEP}${RESET}${segment2}${DIM} ${SEP}${RESET}${segment3}${DIM} ${SEP}${RESET}${segment4}${DIM} ${SEP}${RESET}${segment5}"
