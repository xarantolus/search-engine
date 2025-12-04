import argparse
import json
import sys
from datetime import datetime, timedelta
from typing import List, Tuple, Optional
import matplotlib.pyplot as plt
import matplotlib.dates as mdates
import numpy as np
import pandas as pd


def parse_date(date_str: str) -> datetime:
    """Parse date string in ISO format."""
    try:
        # First try with timezone info
        dt = datetime.fromisoformat(date_str.replace('Z', '+00:00'))
        # Convert to naive datetime (remove timezone info)
        if dt.tzinfo is not None:
            dt = dt.replace(tzinfo=None)
        return dt
    except ValueError:
        # Try without timezone info
        try:
            return datetime.fromisoformat(date_str.replace('Z', ''))
        except ValueError:
            # Try parsing just the date part (YYYY-MM-DD)
            return datetime.strptime(date_str[:10], '%Y-%m-%d')

def load_json_data(filepath: str) -> dict:
    """Load and parse JSON data from file."""
    try:
        with open(filepath, 'r') as f:
            return json.load(f)
    except FileNotFoundError:
        print(f"Error: File {filepath} not found", file=sys.stderr)
        sys.exit(1)
    except json.JSONDecodeError as e:
        print(f"Error: Invalid JSON in {filepath}: {e}", file=sys.stderr)
        sys.exit(1)


def detect_outliers(counts: List[int], method: str = 'iqr') -> Tuple[List[int], int]:
    """
    Detect and cap outliers in the data.

    Args:
        counts: List of count values
        method: 'iqr' for Interquartile Range or 'zscore' for Z-score method

    Returns:
        Tuple of (cleaned_counts, outlier_threshold)
    """
    if not counts:
        return counts, 0

    counts_array = np.array(counts)

    # Only consider non-zero values for outlier detection
    non_zero_counts = counts_array[counts_array > 0]

    if len(non_zero_counts) == 0:
        # All values are zero, no outliers to detect
        return counts, 0

    if method == 'iqr':
        # Interquartile Range method on non-zero values
        q1 = np.percentile(non_zero_counts, 25)
        q3 = np.percentile(non_zero_counts, 75)
        iqr = q3 - q1

        # Define outlier threshold (1.5 * IQR is standard, but we can be more conservative)
        lower_bound = q1 - 1.5 * iqr
        upper_bound = q3 + 1.5 * iqr

        # Cap outliers at the upper bound, but preserve zeros
        cleaned_counts = np.where(
            (counts_array > 0) & (counts_array > upper_bound),
            upper_bound,
            counts_array
        )
        threshold = int(upper_bound)

    elif method == 'zscore':
        # Z-score method (cap at 3 standard deviations) on non-zero values
        mean = np.mean(non_zero_counts)
        std = np.std(non_zero_counts)

        if std == 0:
            # All non-zero values are the same, no outliers
            return counts, int(mean) if mean > 0 else max(counts)

        threshold = mean + 3 * std

        # Apply Z-score capping only to non-zero values
        z_scores = np.abs((non_zero_counts - mean) / std)
        cleaned_counts = np.where(
            (counts_array > 0) & (counts_array > threshold),
            threshold,
            counts_array
        )
        threshold = int(threshold)

    else:
        # Percentile method - cap at 99th percentile of non-zero values
        threshold = int(np.percentile(non_zero_counts, 99))
        cleaned_counts = np.where(
            (counts_array > 0) & (counts_array > threshold),
            threshold,
            counts_array
        )

    num_outliers = np.sum((counts_array > 0) & (counts_array > threshold))
    if num_outliers > 0:
        print(f"Detected {num_outliers} outliers (non-zero values > {threshold}), capped at {threshold}")
        print(f"Zero values preserved: {np.sum(counts_array == 0)}")

    return cleaned_counts.tolist(), threshold

def aggregate_data(dates: List[datetime], counts: List[int], period: str = 'week') -> Tuple[List[datetime], List[int]]:
    """
    Aggregate data by week or month.

    Args:
        dates: List of datetime objects
        counts: List of count values
        period: 'week' or 'month' for aggregation period

    Returns:
        Tuple of (aggregated_dates, aggregated_counts)
    """
    if not dates or not counts:
        return dates, counts

    # Create a pandas DataFrame for easier aggregation
    df = pd.DataFrame({'date': dates, 'count': counts})
    df['date'] = pd.to_datetime(df['date'])

    if period == 'week':
        # Group by week (Sunday to Saturday)
        df_grouped = df.groupby(pd.Grouper(key='date', freq='W-SUN')).agg({
            'count': 'sum'
        }).reset_index()
        period_label = 'Weekly'
    elif period == 'month':
        # Group by month
        df_grouped = df.groupby(pd.Grouper(key='date', freq='MS')).agg({
            'count': 'sum'
        }).reset_index()
        period_label = 'Monthly'
    else:
        # Return original data for daily
        return dates, counts

    # Convert back to lists
    agg_dates = df_grouped['date'].dt.to_pydatetime().tolist()
    agg_counts = df_grouped['count'].tolist()

    print(f"Aggregated to {period_label}: {len(agg_dates)} data points")

    return agg_dates, agg_counts


def filter_data_by_date_range(
    start_date: datetime,
    counts: List[int],
    filter_start: Optional[str] = None,
    filter_end: Optional[str] = None
) -> Tuple[List[datetime], List[int]]:
    """Filter counts data by date range."""

    # Generate all dates from the start date
    dates = [start_date + timedelta(days=i) for i in range(len(counts))]

    # Apply filtering if specified
    filtered_dates = []
    filtered_counts = []

    for i, date in enumerate(dates):
        # Check if date is within filter range
        if filter_start:
            filter_start_dt = parse_date(filter_start)
            if date < filter_start_dt:
                continue

        if filter_end:
            filter_end_dt = parse_date(filter_end)
            if date > filter_end_dt:
                continue

        filtered_dates.append(date)
        filtered_counts.append(counts[i])

    return filtered_dates, filtered_counts


def create_graph(dates: List[datetime], counts: List[int], output_file: Optional[str] = None, period: str = 'day'):
    """Create and display/save the graph."""
    # Use a modern style and larger figure
    plt.style.use('default')
    fig, ax = plt.subplots(figsize=(16, 10))

    # Set background colors
    fig.patch.set_facecolor('white')
    ax.set_facecolor('#f8f9fa')

    # Create the plot with better styling
    if len(dates) > 500:
        # Use a gradient-like effect for large datasets
        line = ax.plot(dates, counts, linewidth=1.5, color='#2E86AB', alpha=0.8, zorder=3)
        # Add area fill for better visual impact
        ax.fill_between(dates, counts, alpha=0.3, color='#A23B72', zorder=2)
    else:
        # For smaller datasets, use markers
        ax.plot(dates, counts, linewidth=2, color='#2E86AB', marker='o',
                markersize=4, markerfacecolor='#F18F01', markeredgecolor='#2E86AB',
                markeredgewidth=1, zorder=3)
        ax.fill_between(dates, counts, alpha=0.2, color='#A23B72', zorder=2)

    # Enhanced title and labels
    period_title = {'day': 'Daily', 'week': 'Weekly', 'month': 'Monthly'}
    title = f'{period_title.get(period, "Daily")} Edit Activity Over Time'
    ax.set_title(title, fontsize=20, fontweight='bold', pad=20, color='#2c3e50')
    ax.set_xlabel('Date', fontsize=14, fontweight='medium', color='#34495e')
    ax.set_ylabel('Number of Edits', fontsize=14, fontweight='medium', color='#34495e')

    # Enhanced grid
    ax.grid(True, alpha=0.4, linestyle='-', linewidth=0.5, color='#bdc3c7')
    ax.set_axisbelow(True)

    # Smart date formatting based on data span and aggregation period
    date_span = (dates[-1] - dates[0]).days

    if period == 'month' or date_span > 3650:  # Monthly data or more than 10 years
        ax.xaxis.set_major_formatter(mdates.DateFormatter('%Y'))
        ax.xaxis.set_major_locator(mdates.YearLocator(base=2 if date_span > 7300 else 1))
        ax.xaxis.set_minor_locator(mdates.YearLocator())
    elif period == 'week' or date_span > 1095:  # Weekly data or more than 3 years
        ax.xaxis.set_major_formatter(mdates.DateFormatter('%Y'))
        ax.xaxis.set_major_locator(mdates.YearLocator())
        ax.xaxis.set_minor_locator(mdates.MonthLocator(interval=6))
    elif date_span > 365:  # More than 1 year
        ax.xaxis.set_major_formatter(mdates.DateFormatter('%Y-%m'))
        ax.xaxis.set_major_locator(mdates.MonthLocator(interval=3))
        ax.xaxis.set_minor_locator(mdates.MonthLocator())
    else:
        # Show month-day for shorter periods
        ax.xaxis.set_major_formatter(mdates.DateFormatter('%m-%d'))
        ax.xaxis.set_major_locator(mdates.MonthLocator())
        ax.xaxis.set_minor_locator(mdates.WeekdayLocator())

    # Style the tick labels
    plt.xticks(rotation=45, fontsize=11, color='#34495e')
    plt.yticks(fontsize=11, color='#34495e')

    # Add subtle borders
    for spine in ax.spines.values():
        spine.set_edgecolor('#bdc3c7')
        spine.set_linewidth(1)

    # Calculate statistics
    mean_edits = np.mean(counts)
    median_edits = np.median(counts)
    max_edits = max(counts)
    min_edits = min(counts)
    total_edits = sum(counts)
    non_zero_days = sum(1 for c in counts if c > 0)

    # Create a beautiful statistics box
    stats_text = (
        f"📊 Statistics\n"
        f"Total Edits: {total_edits:,}\n"
        f"Active {period}s: {non_zero_days:,} ({non_zero_days/len(counts)*100:.1f}%)\n"
        f"Mean: {mean_edits:.1f} edits/{period}\n"
        f"Median: {median_edits:.1f} edits/{period}\n"
        f"Peak: {max_edits} edits\n"
        f"Range: {min_edits} - {max_edits}"
    )

    # Position the stats box in a nice location
    props = dict(boxstyle='round,pad=0.5', facecolor='white', alpha=0.9, edgecolor='#bdc3c7')
    ax.text(0.02, 0.98, stats_text, transform=ax.transAxes, fontsize=10,
            verticalalignment='top', bbox=props, fontfamily='monospace', color='#2c3e50')

    # Add a subtle watermark with date range
    date_range_text = f"📅 {dates[0].strftime('%Y-%m-%d')} to {dates[-1].strftime('%Y-%m-%d')}"
    ax.text(0.98, 0.02, date_range_text, transform=ax.transAxes, fontsize=9,
            verticalalignment='bottom', horizontalalignment='right',
            alpha=0.7, style='italic', color='#7f8c8d')

    # Add trend line for large datasets
    if len(dates) > 100 and period != 'day':
        # Calculate simple moving average
        window_size = max(3, len(counts) // 20)
        if len(counts) >= window_size:
            moving_avg = np.convolve(counts, np.ones(window_size)/window_size, mode='valid')
            avg_dates = dates[window_size-1:]
            ax.plot(avg_dates, moving_avg, '--', linewidth=2, color='#E74C3C',
                   alpha=0.8, label=f'{window_size}-{period} moving average', zorder=4)
            ax.legend(loc='upper right', frameon=True, fancybox=True, shadow=True,
                     fontsize=10, facecolor='white', edgecolor='#bdc3c7')

    # Enhance layout
    plt.tight_layout()

    # Add some padding around the plot
    plt.subplots_adjust(left=0.08, right=0.95, top=0.92, bottom=0.12)

    # Save or show the plot
    if output_file:
        plt.savefig(output_file, dpi=300, bbox_inches='tight', facecolor='white',
                   edgecolor='none', format='png' if output_file.endswith('.png') else None)
        print(f"📈 Beautiful graph saved to {output_file}")
    else:
        plt.show()

    plt.close()

def main():
    parser = argparse.ArgumentParser(
        description='Generate a graph of edits over time from JSON data'
    )
    parser.add_argument(
        'input_file',
        help='Path to the JSON input file'
    )
    parser.add_argument(
        '--start-date',
        help='Filter start date (ISO format, e.g., 2000-01-01 or 2000-01-01T00:00:00Z)'
    )
    parser.add_argument(
        '--end-date',
        help='Filter end date (ISO format, e.g., 2023-12-31 or 2023-12-31T23:59:59Z)'
    )
    parser.add_argument(
        '--output',
        '-o',
        help='Output file path for the graph (if not specified, graph will be displayed)'
    )
    parser.add_argument(
        '--outlier-method',
        choices=['iqr', 'zscore', 'percentile'],
        default='iqr',
        help='Method for handling outliers (default: iqr)'
    )
    parser.add_argument(
        '--no-outlier-filtering',
        action='store_true',
        help='Disable outlier filtering'
    )
    parser.add_argument(
        '--aggregate',
        choices=['day', 'week', 'month'],
        default='day',
        help='Aggregate data by time period (default: day)'
    )

    args = parser.parse_args()

    # Load JSON data
    data = load_json_data(args.input_file)

    # Validate JSON structure
    if 'modificationDateStats' not in data:
        print("Error: JSON must contain 'modificationDateStats' key", file=sys.stderr)
        sys.exit(1)

    mod_stats = data['modificationDateStats']

    if 'startDate' not in mod_stats or 'counts' not in mod_stats:
        print("Error: 'modificationDateStats' must contain 'startDate' and 'counts'", file=sys.stderr)
        sys.exit(1)

    # Parse start date and counts
    try:
        start_date = parse_date(mod_stats['startDate'])
        counts = mod_stats['counts']
    except Exception as e:
        print(f"Error parsing data: {e}", file=sys.stderr)
        sys.exit(1)

    # Handle outliers unless disabled
    if not args.no_outlier_filtering:
        original_max = max(counts) if counts else 0
        counts, threshold = detect_outliers(counts, args.outlier_method)
        if original_max > threshold:
            print(f"Original max: {original_max}, capped at: {threshold}")

    # Filter data by date range if specified
    filtered_dates, filtered_counts = filter_data_by_date_range(
        start_date, counts, args.start_date, args.end_date
    )

    if not filtered_dates:
        print("No data points after filtering", file=sys.stderr)
        sys.exit(1)

    # Aggregate data if requested
    if args.aggregate != 'day':
        filtered_dates, filtered_counts = aggregate_data(filtered_dates, filtered_counts, args.aggregate)

    print(f"Loaded {len(counts)} data points")
    print(f"After filtering and aggregation: {len(filtered_counts)} data points")
    print(f"Date range: {filtered_dates[0].strftime('%Y-%m-%d')} to {filtered_dates[-1].strftime('%Y-%m-%d')}")

    # Print some basic statistics
    print(f"Statistics - Mean: {np.mean(filtered_counts):.1f}, "
          f"Median: {np.median(filtered_counts):.1f}, "
          f"Max: {max(filtered_counts)}, "
          f"Min: {min(filtered_counts)}")

    # Create and display/save the graph
    create_graph(filtered_dates, filtered_counts, args.output, args.aggregate)


if __name__ == '__main__':
    main()
